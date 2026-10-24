package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	log "github.com/sirupsen/logrus"
)

// medianValue returns the median of one nullable integer column under the supplied filter. The
// filter must already exclude NULL values for that column. Callers hold the store read lock.
func (s *SQLiteStore) medianValue(ctx context.Context, filter string, args []any, column string) (*float64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events "+filter, args...).Scan(&count); err != nil {
		return nil, fmt.Errorf("count %s samples: %w", column, err)
	}
	if count == 0 {
		return nil, nil
	}
	// Even sample counts average the two centre rows; odd counts select the single middle row twice.
	medianArgs := append(append([]any(nil), args...), 2-count%2, (count-1)/2)
	var median float64
	if err := s.db.QueryRowContext(ctx, "SELECT AVG(value) FROM (SELECT "+column+" AS value FROM events "+filter+" ORDER BY "+column+" LIMIT ? OFFSET ?)", medianArgs...).Scan(&median); err != nil {
		return nil, fmt.Errorf("query %s median: %w", column, err)
	}
	return &median, nil
}

// timingMetrics runs under the caller's store read lock. Column names are fixed,
// never supplied by a query. Provider timestamps cannot establish clock offsets.
func (s *SQLiteStore) timingMetrics(ctx context.Context, query model.Query, percentiles bool) (map[string]model.TimingMetric, error) {
	where, args, err := buildWhere(query)
	if err != nil {
		return nil, err
	}
	items := []struct{ name, column, source string }{
		{"e2e", "latency_ms", "observed"},
		{"latency", "first_token_latency_ms", "observed_dispatch_to_first_token"},
		{"provider_latency", "provider_latency_ms", "observed_dispatch_to_response"},
		{"ttft", "time_to_first_token_ms", "observed"},
		{"generation", "generation_time_ms", "observed_first_to_last_arrival"},
	}
	result := map[string]model.TimingMetric{}
	if !percentiles {
		selects := make([]string, 0, len(items)*3)
		for _, item := range items {
			selects = append(selects, "COUNT("+item.column+")", "SUM("+item.column+")", "MAX("+item.column+")")
		}
		var values [5]struct {
			count int64
			total sql.NullInt64
			max   sql.NullInt64
		}
		scanArgs := make([]any, 0, len(values)*3)
		for index := range values {
			scanArgs = append(scanArgs, &values[index].count, &values[index].total, &values[index].max)
		}
		if err := s.db.QueryRowContext(ctx, "SELECT "+strings.Join(selects, ",")+" FROM events "+where, args...).Scan(scanArgs...); err != nil {
			return nil, fmt.Errorf("query analytics timing metrics: %w", err)
		}
		for index, item := range items {
			metric := model.TimingMetric{Source: item.source, SampleCount: values[index].count}
			if values[index].total.Valid {
				metric.TotalMS = &values[index].total.Int64
				metric.MaxMS = &values[index].max.Int64
			}
			result[item.name] = metric
		}
		return result, nil
	}
	for _, item := range items {
		metric := model.TimingMetric{Source: item.source}
		var total, maximum sql.NullInt64
		filter := where + " AND " + item.column + " IS NOT NULL"
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*),SUM("+item.column+"),MAX("+item.column+") FROM events "+filter, args...).Scan(&metric.SampleCount, &total, &maximum); err != nil {
			return nil, fmt.Errorf("query %s timing: %w", item.name, err)
		}
		if total.Valid {
			metric.TotalMS = &total.Int64
			metric.MaxMS = &maximum.Int64
		}
		if percentiles && metric.SampleCount > 0 {
			var p95 int64
			percentileArgs := append(append([]any(nil), args...), percentileOffset(metric.SampleCount, 95, 100))
			if err := s.db.QueryRowContext(ctx, "SELECT "+item.column+" FROM events "+filter+" ORDER BY "+item.column+" LIMIT 1 OFFSET ?", percentileArgs...).Scan(&p95); err != nil {
				return nil, err
			}
			p95Value := float64(p95)
			metric.P95MS = &p95Value
			median, err := s.medianValue(ctx, filter, args, item.column)
			if err != nil {
				return nil, err
			}
			metric.MedianMS = median
		}
		result[item.name] = metric
	}
	return result, nil
}

func processingTime(metrics map[string]model.TimingMetric, attempts int64) model.ProcessingTime {
	e2e, ttft, generation := metrics["e2e"], metrics["ttft"], metrics["generation"]
	return model.ProcessingTime{E2EMS: e2e.TotalMS, TTFTMS: ttft.TotalMS, GenerationMS: generation.TotalMS,
		SampleCount: e2e.SampleCount, TTFTSampleCount: ttft.SampleCount, GenerationSampleCount: generation.SampleCount,
		LatencyMS: metrics["latency"].TotalMS, ProviderLatencyMS: metrics["provider_latency"].TotalMS,
		LatencySampleCount: metrics["latency"].SampleCount, ProviderLatencySampleCount: metrics["provider_latency"].SampleCount,
		Partial: e2e.SampleCount < attempts || ttft.SampleCount < attempts || generation.SampleCount < attempts || metrics["latency"].SampleCount < attempts || metrics["provider_latency"].SampleCount < attempts}
}

// keyUsageDetails combines retained token totals with raw timing observations.
// Retained aggregates do not reconstruct timing observations that were never captured.
func (s *SQLiteStore) keyUsageDetails(ctx context.Context, query model.Query) (map[string]model.KeyIdentity, error) {
	rawWhere, rawArgs, err := buildWhere(query)
	if err != nil {
		return nil, err
	}
	retainedWhere, retainedArgs, err := buildRollupWhere(query, "bucket_start_ns", "bucket_end_ns")
	if err != nil {
		return nil, err
	}
	args := append(rawArgs, retainedArgs...)
	rows, err := s.db.QueryContext(ctx, `SELECT key_id,model,SUM(tokens),COALESCE(SUM(cost),0),SUM(generation),SUM(samples) FROM (
 SELECT key_id,model,SUM(total_tokens) AS tokens,SUM(known_cost_nano) AS cost,SUM(generation_time_ms) AS generation,COUNT(generation_time_ms) AS samples FROM events `+rawWhere+` GROUP BY key_id,model
 UNION ALL SELECT key_id,model,SUM(total_tokens),SUM(known_cost_nano),NULL,0 FROM rollups `+retainedWhere+` GROUP BY key_id,model
 ) GROUP BY key_id,model ORDER BY key_id,SUM(tokens) DESC,model`, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.WithError(err).Error("close analytics key timing rows")
		}
	}()
	result := map[string]model.KeyIdentity{}
	for rows.Next() {
		var keyID, name string
		var tokens, count int64
		var cost model.NanoUSD
		var generation sql.NullInt64
		if err := rows.Scan(&keyID, &name, &tokens, &cost, &generation, &count); err != nil {
			return nil, err
		}
		item := result[keyID]
		if item.TopModel == nil {
			item.TopModel = &name
			item.TopModelTokens = tokens
		}
		// Rows arrive in token order, so a strict comparison keeps the token tie-break for cost.
		if cost > item.TopCostModelUSD {
			costModel := name
			item.TopCostModel, item.TopCostModelUSD = &costModel, cost
		}
		if generation.Valid {
			total := generation.Int64
			if item.GenerationTimeMS != nil {
				total += *item.GenerationTimeMS
			}
			item.GenerationTimeMS = &total
		}
		item.GenerationSampleCount += count
		result[keyID] = item
	}
	return result, rows.Err()
}
