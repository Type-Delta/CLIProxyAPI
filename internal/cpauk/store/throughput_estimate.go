package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// Keep these expressions identical to migration 014 so SQLite can use both the
// cohort equality prefix and the completion range without sorting a day's rows.
const arrivalEstimateCompletionSQL = `max(requested_at_ns + latency_ms * 1000000,
        coalesce(responded_at_ns, requested_at_ns + latency_ms * 1000000))`

const arrivalEstimatePredicateSQL = `succeeded = 1 AND import_batch_id IS NULL
    AND accounting_schema = 'normalized-v1' AND token_quality IN ('exact', 'estimated')
    AND output_tokens > 0 AND input_tokens <= total_tokens
    AND output_tokens <= total_tokens - input_tokens AND reasoning_tokens <= output_tokens
    AND latency_ms > 0 AND latency_ms <= 9223372036854
    AND requested_at_ns <= 9223372036854775807 - latency_ms * 1000000
    AND output_timing IS NOT NULL
    AND json_extract(output_timing, '$.version') = 1
    AND json_extract(output_timing, '$.status') = 'observed'
    AND json_extract(output_timing, '$.last_arrival_ns') - json_extract(output_timing, '$.first_arrival_ns') >= 1000000000
    AND json_extract(output_timing, '$.window_tokens') >= 64
    AND json_extract(output_timing, '$.arrival_count') >= 10
    AND json_extract(output_timing, '$.token_quality') IN ('exact', 'estimated')`

const arrivalEstimateHistorySQL = `SELECT attempt_id, proxy_request_id, requested_at_ns,
latency_ms, responded_at_ns, input_tokens, output_tokens, reasoning_tokens, total_tokens,
token_quality, output_timing FROM events INDEXED BY events_arrival_estimate_idx
WHERE ` + arrivalEstimatePredicateSQL + `
AND key_id = ? AND provider = ? AND model = ? AND executor_type = ?
AND credential_id = ? AND credential_id_algorithm = ?
AND json_extract(output_timing, '$.protocol') = ?
AND json_extract(output_timing, '$.source') = ?
AND json_extract(output_timing, '$.coverage') = ?
AND stream = ? AND reasoning_effort IS ? AND service_tier_requested IS ? AND service_tier_used IS ?
AND ` + arrivalEstimateCompletionSQL + ` >= ? AND ` + arrivalEstimateCompletionSQL + ` < ?
AND requested_at_ns >= ?
ORDER BY ` + arrivalEstimateCompletionSQL + ` DESC, proxy_request_id ASC, attempt_id ASC`

func arrivalEstimateArguments(target model.Event) []any {
	timing := target.OutputTiming
	start := target.RequestedAt.Add(-24 * time.Hour).UnixNano()
	return []any{target.KeyID, target.Provider, target.Model, target.ExecutorType,
		target.CredentialID, target.CredentialIDAlgorithm, timing.Protocol, timing.Source, timing.Coverage,
		target.Stream, target.ReasoningEffort, target.ServiceTierRequested, target.ServiceTierUsed,
		start, target.RequestedAt.UnixNano(), start}
}

// enrichArrivalEstimates runs under the caller's read lock, after its event rows
// have been consumed. An optional history failure never fails an accounting read.
func (s *SQLiteStore) enrichArrivalEstimates(ctx context.Context, events []model.Event) {
	results := make(map[string]model.ThroughputEstimate)
	for i := range events {
		event := &events[i]
		event.ThroughputEstimate = nil
		estimate := model.EstimateArrivalThroughput(*event, nil)
		if estimate.Reason == "observed_available" {
			continue
		}
		if estimate.Reason == "insufficient_history" {
			arguments := arrivalEstimateArguments(*event)
			cacheKey, _ := json.Marshal(append(arguments, event.ProxyRequestID))
			if cached, ok := results[string(cacheKey)]; ok {
				estimate = cached
			} else {
				history, err := s.arrivalEstimateHistory(ctx, *event, arguments)
				if err != nil {
					estimate.Reason = "history_unavailable"
				} else {
					estimate = model.EstimateArrivalThroughput(*event, history)
				}
				results[string(cacheKey)] = estimate
			}
		}
		event.ThroughputEstimate = &estimate
	}
}

func (s *SQLiteStore) arrivalEstimateHistory(ctx context.Context, target model.Event, arguments []any) ([]model.Event, error) {
	rows, err := s.db.QueryContext(ctx, arrivalEstimateHistorySQL, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	history := make([]model.Event, 0, 200)
	seen := make(map[string]struct{}, 200)
	for rows.Next() {
		// Cohort identity is fixed by SQL. Select only label evidence, avoiding
		// upstream payload bodies and other unrelated event diagnostics.
		event := target
		var requestedNS int64
		var respondedNS sql.NullInt64
		var quality, timing string
		errScan := rows.Scan(&event.AttemptID, &event.ProxyRequestID, &requestedNS, &event.LatencyMS,
			&respondedNS, &event.Tokens.Input, &event.Tokens.Output, &event.Tokens.Reasoning,
			&event.Tokens.Total, &quality, &timing)
		if errScan != nil {
			return nil, errScan
		}
		event.RequestedAt, event.RespondedAt = time.Unix(0, requestedNS).UTC(), scanNullableTime(respondedNS)
		event.Tokens.Quality = model.TokenQuality(quality)
		event.ImportBatchID, event.Source = "", "native"
		event.OutputTiming = &model.OutputTiming{}
		if errDecode := json.Unmarshal([]byte(timing), event.OutputTiming); errDecode != nil {
			continue
		}
		if _, _, ok := event.ArrivalEstimateObservation(); !ok {
			continue
		}
		if event.ProxyRequestID == target.ProxyRequestID {
			continue
		}
		if _, exists := seen[event.ProxyRequestID]; exists {
			continue
		}
		seen[event.ProxyRequestID] = struct{}{}
		history = append(history, event)
		if len(history) == 200 {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return history, nil
}
