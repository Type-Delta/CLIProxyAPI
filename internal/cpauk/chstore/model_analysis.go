package chstore

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// analysisModels aggregates accounting and exact identities at both model and
// model/bucket levels in one query. Identity-only retained rows carry no amounts.
// Per-bucket exact states merge across models or time without summing distinct counts.
func (s *Store) analysisModels(ctx context.Context, q model.Query) (model.AnalysisModelByTime, error) {
	return s.analysisAccounting(ctx, q, nil)
}

// A non-nil series collects category buckets in the same scan as the models.
func (s *Store) analysisAccounting(ctx context.Context, q model.Query, series *model.AnalysisSeriesByCategory) (model.AnalysisModelByTime, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ctx = withReadCutoffs(ctx)
	partial, err := s.rawRangePartial(ctx, q)
	if err != nil {
		return model.AnalysisModelByTime{}, err
	}
	width := analysisBucketWidth(q)
	sequence, err := analyticsBucketSequence(q, width)
	if err != nil {
		return model.AnalysisModelByTime{}, err
	}
	if partial && series == nil {
		dq := q
		dq.Operation = model.OperationDimensions
		dq.BucketWidth = ""
		dq.Window = ""
		if err := s.validateRetainedRange(ctx, dq); err != nil {
			return model.AnalysisModelByTime{}, fmt.Errorf("query analysis models: %w", err)
		}
		tq := q
		tq.Operation = model.OperationTimeseries
		tq.Window = ""
		tq.BucketWidth = width
		if err := s.validateRetainedRange(ctx, tq); err != nil {
			return model.AnalysisModelByTime{}, fmt.Errorf("query analysis model buckets: %w", err)
		}
	}
	if series != nil {
		tq := q
		tq.Operation = model.OperationTimeseries
		tq.Window = ""
		tq.BucketWidth = width
		if err := s.validateQuery(&tq, model.OperationTimeseries); err != nil {
			return model.AnalysisModelByTime{}, err
		}
		if err := s.validateAnalysisRange(ctx, tq); err != nil {
			return model.AnalysisModelByTime{}, err
		}
	}
	q.BucketWidth = width
	fields := "input_tokens,output_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,reasoning_tokens,total_tokens,known_cost_nano,unpriced_tokens"
	raw, err := s.rawSelection(ctx, q, append([]string{"requested_at_ns", "proxy_request_id", "model", "generation_time_ms", "outcome", "succeeded"}, strings.Split(fields, ",")...)...)
	if err != nil {
		return model.AnalysisModelByTime{}, err
	}
	generation := "generation_time_ms"
	if partial {
		generation = "if(outcome='canceled',NULL,generation_time_ms)"
	}
	source := "SELECT requested_at_ns AS at_ns,model,proxy_request_id,toInt64(1) AS upstream_attempts,succeeded," + fields + "," + generation + " AS observed_generation_ms FROM (" + raw.SQL + ")"
	args := raw.Args
	if partial {
		facts, err := s.retainedSelection(ctx, q, false, "bucket_start_ns AS at_ns", "model", "CAST(NULL AS Nullable(String)) AS proxy_request_id", "upstream_attempts", "succeeded", fields, "CAST(NULL AS Nullable(Int64)) AS observed_generation_ms")
		if err != nil {
			return model.AnalysisModelByTime{}, err
		}
		ids, err := s.retainedSelection(ctx, q, true, "bucket_start_ns", "model", "proxy_request_id")
		if err != nil {
			return model.AnalysisModelByTime{}, err
		}
		ids.SQL = "SELECT bucket_start_ns AS at_ns,model,proxy_request_id,toInt64(0),toUInt8(0),toInt64(0),toInt64(0),toInt64(0),toInt64(0),toInt64(0),toInt64(0),toInt64(0),toInt64(0),toInt64(0),CAST(NULL AS Nullable(Int64)) FROM (" + ids.SQL + ")"

		source += " UNION ALL " + facts.SQL + " UNION ALL " + ids.SQL
		args = append(args, facts.Args...)
		args = append(args, ids.Args...)
	}
	groups := "(model),(model,bucket)"
	if series != nil {
		groups += ",(bucket)"
	}
	statement := `WITH per_bucket AS (SELECT model,` + coreBucketExpression("at_ns", q) + ` AS bucket,uniqExactState(proxy_request_id) AS request_ids,
 sum(input_tokens) AS input,sum(output_tokens) AS output,sum(cached_tokens) AS cached,sum(cache_read_tokens) AS cache_read,sum(cache_creation_tokens) AS cache_creation,sum(reasoning_tokens) AS reasoning,sum(total_tokens) AS token_total,
 sum(toInt128(coalesce(known_cost_nano,0))) AS cost,sum(unpriced_tokens) AS unpriced,sumOrNull(observed_generation_ms) AS generation_total,toInt64(count(observed_generation_ms)) AS samples,sumIf(upstream_attempts,succeeded=1) AS succeeded_count,sumIf(upstream_attempts,succeeded=0) AS failed_count
 FROM (` + source + `) GROUP BY model,bucket),
 grouped AS (SELECT model,bucket,grouping(bucket) AS overall,grouping(model) AS category,toInt64(uniqExactMerge(request_ids)) AS requests,
 sum(input) AS input,sum(output) AS output,sum(cached) AS cached,sum(cache_read) AS cache_read,sum(cache_creation) AS cache_creation,sum(reasoning) AS reasoning,sum(token_total) AS token_total,accurateCast(sum(cost),'Int64') AS cost,sum(unpriced) AS unpriced,
 sumOrNull(generation_total) AS generation_total,sum(samples) AS samples,sum(succeeded_count) AS succeeded_count,sum(failed_count) AS failed_count
 FROM per_bucket GROUP BY GROUPING SETS (` + groups + `)),
 totals AS (SELECT *,sumIf(token_total,overall=1) OVER (PARTITION BY category,model) AS model_tokens FROM grouped),
 ranked AS (SELECT *,dense_rank() OVER (ORDER BY category ASC,model_tokens DESC,model ASC) AS model_rank FROM totals)
 SELECT model,bucket,overall,category,requests,input,output,cached,cache_read,cache_creation,reasoning,token_total,cost,unpriced,generation_total,samples,succeeded_count,failed_count FROM ranked WHERE category=1 OR model_rank<=10 ORDER BY overall DESC,`
	if partial {
		statement += "model_rank,bucket"
	} else {
		statement += "if(overall=1,model_rank,0),model,bucket"
	}
	rows, err := s.db.Conn().Query(ctx, statement, args...)
	if err != nil {
		return model.AnalysisModelByTime{}, err
	}
	defer func() { _ = rows.Close() }()
	result := model.AnalysisModelByTime{Models: []model.AnalysisModel{}}
	buckets := map[int64][]model.AnalysisModel{}
	categories := map[int64]model.ActivityBucket{}
	for rows.Next() {
		var item model.AnalysisModel
		var bucket, cost int64
		var overall, category uint64
		var succeeded, failed int64
		if err := rows.Scan(&item.Model, &bucket, &overall, &category, &item.Requests, &item.InputTokens, &item.OutputTokens, &item.CachedTokens, &item.CacheReadTokens, &item.CacheCreationTokens, &item.ReasoningTokens, &item.TotalTokens, &cost, &item.UnpricedTokens, &item.GenerationTimeMS, &item.GenerationSampleCount, &succeeded, &failed); err != nil {
			return result, err
		}
		item.KnownCost = model.NanoUSD(cost)
		if category == 1 {
			categories[bucket] = model.ActivityBucket{Requests: item.Requests, Succeeded: succeeded, Failed: failed, InputTokens: item.InputTokens, OutputTokens: item.OutputTokens, CachedTokens: item.CachedTokens, CacheReadTokens: item.CacheReadTokens, CacheCreationTokens: item.CacheCreationTokens, ReasoningTokens: item.ReasoningTokens, TotalTokens: item.TotalTokens, KnownCost: item.KnownCost, UnpricedTokens: item.UnpricedTokens}
			continue
		}
		if overall == 1 {
			result.Models = append(result.Models, item)
		} else {
			buckets[bucket] = append(buckets[bucket], item)
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	result.Buckets = make([]model.AnalysisModelBucket, 0, len(sequence))
	if series != nil {
		series.Buckets = make([]model.ActivityBucket, 0, len(sequence))
	}
	for _, bounds := range sequence {
		if series != nil {
			item := categories[bounds.start.UnixNano()]
			item.Start = bounds.start
			item.End = bounds.end
			series.Buckets = append(series.Buckets, item)
		}
		bucket := model.AnalysisModelBucket{Start: bounds.start, Models: []model.AnalysisModel{}}
		bucket.Models = append(bucket.Models, buckets[bounds.start.UnixNano()]...)
		result.Buckets = append(result.Buckets, bucket)
		delete(buckets, bounds.start.UnixNano())
	}
	// Fractional DST may produce a second canonical start outside the sequence.
	if partial {
		for start, items := range buckets {
			result.Buckets = append(result.Buckets, model.AnalysisModelBucket{Start: time.Unix(0, start).UTC(), Models: items})
		}
		slices.SortFunc(result.Buckets, func(a, b model.AnalysisModelBucket) int { return a.Start.Compare(b.Start) })
	}
	return result, nil
}

func nullableStringR3(v sql.NullString) *string {
	if v.Valid {
		return &v.String
	}
	return nil
}

// rawRangePartial reports the existing Analysis section coverage contract.
func (s *Store) rawRangePartial(ctx context.Context, q model.Query) (bool, error) {
	if s.db == nil {
		return false, ErrClosed
	}
	b, err := s.readCutoffs(ctx)
	return b.rawSet && q.Start.Before(time.Unix(0, b.raw)), err
}

// validateAnalysisRange reads overlap, partial coverage and retained grains in
// one pass. The error order matches validateRetainedRange.
func (s *Store) validateAnalysisRange(ctx context.Context, q model.Query) error {
	bounds, err := s.readCutoffs(ctx)
	if err != nil {
		return err
	}
	if !bounds.rawSet || q.Start.UnixNano() >= bounds.raw {
		return nil
	}
	selection, err := s.retainedRangeSelection(ctx, q, false, true, "bucket_start_ns", "bucket_end_ns", "grain")
	if err != nil {
		return err
	}
	args := append([]any{q.Start.UnixNano(), q.End.UnixNano()}, selection.Args...)
	args = append(args, q.End.UnixNano(), q.Start.UnixNano())
	var overlapping, partial uint64
	var grains []string
	err = s.db.Conn().QueryRow(ctx, "SELECT count(),countIf(NOT (bucket_start_ns>=? AND bucket_end_ns<=?)),groupUniqArray(grain) FROM ("+selection.SQL+") WHERE bucket_start_ns<? AND bucket_end_ns>?", args...).Scan(&overlapping, &partial, &grains)
	if err != nil {
		return err
	}
	if partial > 0 {
		return ErrRetainedRangePartial
	}
	zone, ok, err := s.metadata(ctx, "retention_time_zone")
	if err != nil {
		return err
	}
	if !ok || zone == "" {
		zone = "UTC"
	}
	if overlapping > 0 && zone != q.TimeZone {
		return RetainedTimeZoneError{StorageTimeZone: zone, QueryTimeZone: q.TimeZone, BucketWidth: q.BucketWidth}
	}
	for _, grain := range grains {
		if err := validateRollupWidth(grain, q.BucketWidth); err != nil {
			return err
		}
	}
	return nil
}
