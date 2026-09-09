package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	log "github.com/sirupsen/logrus"
)

// timingMetrics runs under the caller's store read lock. Column names are fixed,
// never supplied by a query. Provider timestamps cannot establish clock offsets.
func (s *SQLiteStore) timingMetrics(ctx context.Context, query model.Query, percentiles bool) (map[string]model.TimingMetric, error) {
	where, args, err := buildWhere(query)
	if err != nil {
		return nil, err
	}
	result := map[string]model.TimingMetric{}
	for _, item := range []struct{ name, column, source string }{
		{"e2e", "latency_ms", "observed"},
		{"latency", "first_token_latency_ms", "observed_dispatch_to_first_token"},
		{"provider_latency", "provider_latency_ms", "observed_dispatch_to_response"},
		{"ttft", "time_to_first_token_ms", "observed"},
		{"generation", "generation_time_ms", "observed_first_to_last_token"},
	} {
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
			medianArgs := append(append([]any(nil), args...), 2-metric.SampleCount%2, (metric.SampleCount-1)/2)
			var median float64
			if err := s.db.QueryRowContext(ctx, "SELECT AVG(value) FROM (SELECT "+item.column+" AS value FROM events "+filter+" ORDER BY "+item.column+" LIMIT ? OFFSET ?)", medianArgs...).Scan(&median); err != nil {
				return nil, err
			}
			metric.MedianMS = &median
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
	rows, err := s.db.QueryContext(ctx, `SELECT key_id,model,SUM(tokens),SUM(generation),SUM(samples) FROM (
 SELECT key_id,model,SUM(total_tokens) AS tokens,SUM(generation_time_ms) AS generation,COUNT(generation_time_ms) AS samples FROM events `+rawWhere+` GROUP BY key_id,model
 UNION ALL SELECT key_id,model,SUM(total_tokens),NULL,0 FROM rollups `+retainedWhere+` GROUP BY key_id,model
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
		var generation sql.NullInt64
		if err := rows.Scan(&keyID, &name, &tokens, &generation, &count); err != nil {
			return nil, err
		}
		item := result[keyID]
		if item.TopModel == nil {
			item.TopModel = &name
			item.TopModelTokens = tokens
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
