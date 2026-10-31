package chstore

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

type totals struct {
	proxyRequests   int64
	attempts        int64
	succeeded       int64
	failed          int64
	tokens          model.TokenUsage
	knownCost       model.NanoUSD
	unpriced        int64
	exactCacheInput int64
	exactCacheRead  int64
}

type pointState struct {
	point            model.TimeseriesPoint
	requests         map[string]struct{}
	proxyRequests    int64
	proxyRequestsSet bool
	quality          model.TokenQuality
	exactCacheInput  int64
	exactCacheRead   int64
	succeeded        int64
	failed           int64
}

// factsSelection normalizes raw and retained accounting to one weighted row
// contract. It reads no diagnostic JSON, and cannot expose staged rollups.
func (s *Store) factsSelection(ctx context.Context, q model.Query) (readSelection, error) {
	columns := []string{"requested_at_ns", "succeeded", "input_tokens", "output_tokens", "reasoning_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens", "total_tokens", "token_quality", "known_cost_nano", "unpriced_tokens", "generation_time_ms", "outcome"}
	raw, err := s.rawSelection(ctx, q, columns...)
	if err != nil {
		return readSelection{}, err
	}
	fields := "succeeded,input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,token_quality,known_cost_nano,unpriced_tokens"
	retained, err := s.retainedSelection(ctx, q, false, "bucket_start_ns", "upstream_attempts", fields)
	if err != nil {
		return readSelection{}, err
	}
	return readSelection{"SELECT requested_at_ns AS at_ns,toInt64(1) AS upstream_attempts," + fields + ",if(outcome='canceled',NULL,generation_time_ms) AS generation_time_ms FROM (" + raw.SQL + ") UNION ALL SELECT bucket_start_ns AS at_ns,upstream_attempts," + fields + ",CAST(NULL AS Nullable(Int64)) AS generation_time_ms FROM (" + retained.SQL + ")", append(raw.Args, retained.Args...)}, nil
}

const coreTotalsSelect = `toInt64(0),sum(upstream_attempts),sumIf(upstream_attempts,succeeded=1),sumIf(upstream_attempts,succeeded=0),
sum(input_tokens),sum(output_tokens),sum(reasoning_tokens),sum(cached_tokens),sum(cache_read_tokens),sum(cache_creation_tokens),sum(total_tokens),accurateCast(sum(toInt128(coalesce(known_cost_nano,0))),'Int64'),sum(unpriced_tokens),sumIf(input_tokens,token_quality='exact'),sumIf(cache_read_tokens,token_quality='exact'),
if(sumIf(upstream_attempts,token_quality='missing')>0,'missing',if(sumIf(upstream_attempts,token_quality='estimated')>0,'estimated','exact'))`

func coreTotalsArgs(t *totals, quality *string, cost *int64) []any {
	return []any{&t.proxyRequests, &t.attempts, &t.succeeded, &t.failed, &t.tokens.Input, &t.tokens.Output, &t.tokens.Reasoning, &t.tokens.Cached, &t.tokens.CacheRead, &t.tokens.CacheCreation, &t.tokens.Total, cost, &t.unpriced, &t.exactCacheInput, &t.exactCacheRead, quality}
}
func (s *Store) combinedProxyRequests(ctx context.Context, q model.Query) (int64, error) {
	raw, err := s.rawSelection(ctx, q, "proxy_request_id")
	if err != nil {
		return 0, err
	}
	retained, err := s.retainedSelection(ctx, q, true, "proxy_request_id")
	if err != nil {
		return 0, err
	}
	var count uint64
	err = s.db.Conn().QueryRow(ctx, "SELECT uniqExact(proxy_request_id) FROM ("+raw.SQL+" UNION ALL "+retained.SQL+")", append(raw.Args, retained.Args...)...).Scan(&count)
	return int64(count), err
}
func (s *Store) Summary(ctx context.Context, query model.Query) (model.Summary, error) {
	if err := s.validateQuery(&query, model.OperationSummary); err != nil {
		return model.Summary{}, err
	}
	value, err := cachedHistory(s, ctx, query, func() (backend.HistoryCacheValue, error) {
		result, err := s.summaryUncached(ctx, query)
		return backend.HistoryCacheValue{Operation: model.OperationSummary, Summary: result}, err
	})
	return value.Summary, err
}
func (s *Store) summaryUncached(ctx context.Context, query model.Query) (model.Summary, error) {
	if err := s.validateQuery(&query, model.OperationSummary); err != nil {
		return model.Summary{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ctx = withReadCutoffs(ctx)
	if s.db == nil {
		return model.Summary{}, ErrClosed
	}
	if err := s.validateRetainedRange(ctx, query); err != nil {
		return model.Summary{}, err
	}
	facts, err := s.factsSelection(ctx, query)
	if err != nil {
		return model.Summary{}, err
	}
	var result totals
	var quality string
	var cost int64
	if err := s.db.Conn().QueryRow(ctx, "SELECT "+coreTotalsSelect+" FROM ("+facts.SQL+")", facts.Args...).Scan(coreTotalsArgs(&result, &quality, &cost)...); err != nil {
		return model.Summary{}, fmt.Errorf("query analytics summary: %w", err)
	}
	result.knownCost = model.NanoUSD(cost)
	result.tokens.Schema = "normalized-v1"
	result.tokens.Quality = model.TokenQuality(quality)
	result.proxyRequests, err = s.combinedProxyRequests(ctx, query)
	if err != nil {
		return model.Summary{}, err
	}
	timings, err := s.timingMetrics(ctx, query, false)
	if err != nil {
		return model.Summary{}, err
	}
	return model.Summary{
		ProcessingTime: coreProcessingTime(timings, result.attempts), Meta: responseMeta(query),
		ProxyRequests:    result.proxyRequests,
		UpstreamAttempts: result.attempts,
		Succeeded:        result.succeeded,
		Failed:           result.failed,
		SuccessRate:      optionalPercentageFixed(result.succeeded, result.attempts, 2),
		RequestsPerMinute: decimalProductsRatio(
			result.proxyRequests, int64(time.Minute), int64(query.End.Sub(query.Start)), 1),
		TokensPerMinute: decimalProductsRatio(
			result.tokens.Total, int64(time.Minute), int64(query.End.Sub(query.Start)), 1),
		CacheReadRate: optionalCacheReadRate(result.exactCacheRead, result.exactCacheInput),
		RangeDays: decimalRatio(
			int64(query.End.Sub(query.Start)), int64(24*time.Hour)),
		AvgRequestsPerDay: decimalProductsRatio(
			result.proxyRequests, int64(24*time.Hour), int64(query.End.Sub(query.Start)), 1),
		AvgTokensPerDay: decimalProductsRatio(
			result.tokens.Total, int64(24*time.Hour), int64(query.End.Sub(query.Start)), 1),
		AvgKnownCostUSDPerDay: decimalProductsRatio(
			int64(result.knownCost), int64(24*time.Hour), int64(query.End.Sub(query.Start)), 1_000_000_000),
		PriceCoverageComplete: result.unpriced == 0,
		Tokens:                result.tokens,
		KnownCost:             result.knownCost,
		UnpricedTokens:        result.unpriced,
	}, nil
}

func coreProcessingTime(metrics map[string]model.TimingMetric, attempts int64) model.ProcessingTime {
	e2e, ttft, generation := metrics["e2e"], metrics["ttft"], metrics["generation"]
	return model.ProcessingTime{E2EMS: e2e.TotalMS, TTFTMS: ttft.TotalMS, GenerationMS: generation.TotalMS,
		SampleCount: e2e.SampleCount, TTFTSampleCount: ttft.SampleCount, GenerationSampleCount: generation.SampleCount,
		LatencyMS: metrics["latency"].TotalMS, ProviderLatencyMS: metrics["provider_latency"].TotalMS,
		LatencySampleCount: metrics["latency"].SampleCount, ProviderLatencySampleCount: metrics["provider_latency"].SampleCount,
		Partial: e2e.SampleCount < attempts || ttft.SampleCount < attempts || generation.SampleCount < attempts || metrics["latency"].SampleCount < attempts || metrics["provider_latency"].SampleCount < attempts}
}

// bucketExpression matches aggregate.Bucketer. Fixed widths truncate local wall
// time. Calendar widths use the IANA zone, including skipped midnights and DST.
func coreBucketExpression(column string, q model.Query) string {
	zone := "'" + strings.ReplaceAll(q.TimeZone, "'", "''") + "'"
	local := "fromUnixTimestamp64Nano(" + column + "," + zone + ")"
	if q.BucketWidth == "1d" {
		return "toUnixTimestamp64Nano(toDateTime64(toDate32(" + local + "),9," + zone + "))"
	}
	if q.BucketWidth == "1w" {
		return "toUnixTimestamp64Nano(toDateTime64(subtractDays(toDate32(" + local + "),toDayOfWeek(" + local + ")-1),9," + zone + "))"
	}
	duration, _ := time.ParseDuration(q.BucketWidth)
	width := strconv.FormatInt(int64(duration), 10)
	offset := "(toInt64(timezoneOffset(" + local + "))*1000000000)"
	localNS := "(toInt128(" + column + ")+" + offset + ")"
	quotient := "(intDiv(" + localNS + "," + width + ")-if(" + localNS + "<0 AND modulo(" + localNS + "," + width + ")!=0,1,0))"
	return "toInt64(" + quotient + "*" + width + "-" + offset + ")"
}
func (s *Store) Timeseries(ctx context.Context, q model.Query) (model.Timeseries, error) {
	if err := s.validateQuery(&q, model.OperationTimeseries); err != nil {
		return model.Timeseries{}, err
	}
	value, err := cachedHistory(s, ctx, q, func() (backend.HistoryCacheValue, error) {
		result, _, err := s.timeseriesWithOutcomes(ctx, q)
		return backend.HistoryCacheValue{Operation: model.OperationTimeseries, Timeseries: result}, err
	})
	return value.Timeseries, err
}
func (s *Store) timeseriesWithOutcomes(ctx context.Context, q model.Query) (model.Timeseries, map[int64][2]int64, error) {
	if err := s.validateQuery(&q, model.OperationTimeseries); err != nil {
		return model.Timeseries{}, nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ctx = withReadCutoffs(ctx)
	if s.db == nil {
		return model.Timeseries{}, nil, ErrClosed
	}
	if err := s.validateRetainedRange(ctx, q); err != nil {
		return model.Timeseries{}, nil, err
	}
	facts, err := s.factsSelection(ctx, q)
	if err != nil {
		return model.Timeseries{}, nil, err
	}
	rows, err := s.db.Conn().Query(ctx, "SELECT "+coreBucketExpression("at_ns", q)+" AS bucket,"+coreTotalsSelect+",sumOrNull(generation_time_ms),toInt64(count(generation_time_ms)) FROM ("+facts.SQL+") GROUP BY bucket", facts.Args...)
	if err != nil {
		return model.Timeseries{}, nil, err
	}
	points := map[int64]*pointState{}
	bucketer, err := aggregate.NewBucketer(q.TimeZone, q.BucketWidth)
	if err != nil {
		_ = rows.Close()
		return model.Timeseries{}, nil, err
	}
	for rows.Next() {
		var bucket, cost int64
		var data totals
		var quality string
		var generation *int64
		var samples int64
		args := []any{&bucket}
		args = append(args, coreTotalsArgs(&data, &quality, &cost)...)
		args = append(args, &generation, &samples)
		if err := rows.Scan(args...); err != nil {
			_ = rows.Close()
			return model.Timeseries{}, nil, err
		}
		start := time.Unix(0, bucket).UTC()
		var end time.Time
		if q.BucketWidth == "1d" || q.BucketWidth == "1w" {
			_, end, err = bucketer.Bounds(start)
		} else {
			duration, _ := time.ParseDuration(q.BucketWidth)
			end = start.Add(duration)
		}
		if err != nil {
			_ = rows.Close()
			return model.Timeseries{}, nil, err
		}
		points[bucket] = &pointState{point: model.TimeseriesPoint{Start: start, End: end, UpstreamAttempts: data.attempts, Tokens: data.tokens, KnownCost: model.NanoUSD(cost), UnpricedTokens: data.unpriced, GenerationTimeMS: generation, GenerationSampleCount: samples}, quality: model.TokenQuality(quality), exactCacheInput: data.exactCacheInput, exactCacheRead: data.exactCacheRead, succeeded: data.succeeded, failed: data.failed, proxyRequestsSet: true}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return model.Timeseries{}, nil, err
	}
	raw, err := s.rawSelection(ctx, q, "requested_at_ns", "proxy_request_id")
	if err != nil {
		return model.Timeseries{}, nil, err
	}
	retained, err := s.retainedSelection(ctx, q, true, "bucket_start_ns", "proxy_request_id")
	if err != nil {
		return model.Timeseries{}, nil, err
	}
	requests := "SELECT requested_at_ns AS at_ns,proxy_request_id FROM (" + raw.SQL + ") UNION ALL SELECT bucket_start_ns AS at_ns,proxy_request_id FROM (" + retained.SQL + ")"
	rows, err = s.db.Conn().Query(ctx, "SELECT "+coreBucketExpression("at_ns", q)+" AS bucket,toInt64(uniqExact(proxy_request_id)) FROM ("+requests+") GROUP BY bucket", append(raw.Args, retained.Args...)...)
	if err != nil {
		return model.Timeseries{}, nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var bucket, count int64
		if err := rows.Scan(&bucket, &count); err != nil {
			return model.Timeseries{}, nil, err
		}
		if state := points[bucket]; state != nil {
			state.proxyRequests = count
		}
	}
	if err := rows.Err(); err != nil {
		return model.Timeseries{}, nil, err
	}
	return finalizeTimeseries(q, points)
}
func finalizeTimeseries(query model.Query, points map[int64]*pointState) (model.Timeseries, map[int64][2]int64, error) {
	keys := make([]int64, 0, len(points))
	for key := range points {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	result := model.Timeseries{Meta: responseMeta(query), Points: make([]model.TimeseriesPoint, 0, len(keys))}
	outcomes := make(map[int64][2]int64, len(points))
	for _, key := range keys {
		state := points[key]
		if state.proxyRequestsSet {
			state.point.ProxyRequests = state.proxyRequests
		} else {
			state.point.ProxyRequests = int64(len(state.requests))
		}
		state.point.Tokens.Schema = "normalized-v1"
		state.point.Tokens.Quality = state.quality
		if rate := optionalCacheReadRate(state.exactCacheRead, state.exactCacheInput); rate != nil {
			state.point.CacheReadRate = *rate
		}
		result.Points = append(result.Points, state.point)
		outcomes[key] = [2]int64{state.succeeded, state.failed}
	}
	return result, outcomes, nil
}
