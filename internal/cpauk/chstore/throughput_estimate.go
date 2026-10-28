package chstore

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// enrichArrivalEstimates consumes at most 200 unique labels per cohort. SQL
// applies the SQLite index bounds before ordering, deduplicating and limiting.
// Optional history failures never fail an accounting read. Caller holds s.mu.
func (s *Store) enrichArrivalEstimates(ctx context.Context, events []model.Event) {
	s.refreshOutputTiming(ctx, events)
	for i := range events {
		event := &events[i]
		event.EnrichThroughput()
		event.ThroughputEstimate = nil
		estimate := model.EstimateArrivalThroughput(*event, nil)
		if estimate.Reason == "observed_available" {
			continue
		}
		if estimate.Reason == "insufficient_history" {
			history, err := s.arrivalEstimateHistory(ctx, *event)
			if err != nil {
				estimate.Reason = "history_unavailable"
			} else {
				estimate = model.EstimateArrivalThroughput(*event, history)
			}
		}
		event.ThroughputEstimate = &estimate
	}
}

func (s *Store) arrivalEstimateHistory(ctx context.Context, target model.Event) ([]model.Event, error) {
	start := target.RequestedAt.Add(-24 * time.Hour)
	q := model.Query{Start: start, End: target.RequestedAt, TimeZone: "UTC"}
	columns := []string{"attempt_id", "proxy_request_id", "requested_at_ns", "latency_ms", "responded_at_ns", "input_tokens", "output_tokens", "reasoning_tokens", "total_tokens", "token_quality", "output_timing", "succeeded", "outcome", "import_batch_id", "accounting_schema", "key_id", "provider", "model", "executor_type", "credential_id", "credential_id_algorithm", "stream", "reasoning_effort", "service_tier_requested", "service_tier_used"}
	selection, err := s.rawSelection(ctx, q, columns...)
	if err != nil {
		return nil, err
	}
	// OutputTiming is nullable JSON in the scalar schema. Its validity checks are
	// repeated here so malformed evidence cannot consume the bounded label budget.
	j := func(name string) string { return "JSONExtractInt(assumeNotNull(output_timing),'" + name + "')" }
	js := func(name string) string { return "JSONExtractString(assumeNotNull(output_timing),'" + name + "')" }
	completion := "greatest(requested_at_ns+latency_ms*1000000,coalesce(responded_at_ns,requested_at_ns+latency_ms*1000000))"
	predicates := []string{
		"succeeded=1", "(outcome IS NULL OR outcome='succeeded')", "import_batch_id IS NULL", "accounting_schema='normalized-v1'", "token_quality IN ('exact','estimated')",
		"output_tokens>0", "input_tokens>=0", "input_tokens<=total_tokens", "output_tokens<=total_tokens-input_tokens", "reasoning_tokens>=0", "reasoning_tokens<=output_tokens",
		"latency_ms>0", "latency_ms<=9223372036854", "requested_at_ns<=9223372036854775807-latency_ms*1000000", "output_timing IS NOT NULL",
		j("version") + "=1", js("status") + "='observed'", js("reason") + "=''",
		j("first_arrival_ns") + ">=0", j("last_arrival_ns") + "-" + j("first_arrival_ns") + ">=1000000000", j("window_tokens") + ">=64", j("arrival_count") + ">=10", js("token_quality") + " IN ('exact','estimated')",
		j("first_batch_bytes") + ">=0", j("total_bytes") + ">=0", j("largest_batch_bytes") + ">=0", j("max_interarrival_ns") + ">=0", j("first_batch_bytes") + "<=" + j("largest_batch_bytes"), j("largest_batch_bytes") + "<=" + j("total_bytes"), j("max_interarrival_ns") + "<=" + j("last_arrival_ns") + "-" + j("first_arrival_ns"),
		"key_id=?", "provider=?", "model=?", "executor_type=?", "isNotDistinctFrom(credential_id,?)", "isNotDistinctFrom(credential_id_algorithm,?)", js("protocol") + "=?", js("source") + "=?", js("coverage") + "=?",
		"isNotDistinctFrom(stream,?)", "isNotDistinctFrom(reasoning_effort,?)", "isNotDistinctFrom(service_tier_requested,?)", "isNotDistinctFrom(service_tier_used,?)",
		completion + ">=?", completion + "<?", "proxy_request_id<>?", "proxy_request_id<>''",
	}
	timing := target.OutputTiming
	args := append(selection.Args, target.KeyID, target.Provider, target.Model, target.ExecutorType, target.CredentialID, target.CredentialIDAlgorithm, timing.Protocol, timing.Source, timing.Coverage, target.Stream, target.ReasoningEffort, target.ServiceTierRequested, target.ServiceTierUsed, start.UnixNano(), target.RequestedAt.UnixNano(), target.ProxyRequestID)
	statement := `SELECT attempt_id,proxy_request_id,requested_at_ns,latency_ms,responded_at_ns,input_tokens,output_tokens,reasoning_tokens,total_tokens,token_quality,output_timing FROM (` + selection.SQL + `) WHERE ` + strings.Join(predicates, " AND ") + ` ORDER BY ` + completion + ` DESC,proxy_request_id ASC,attempt_id ASC LIMIT 1 BY proxy_request_id LIMIT 200`
	rows, err := s.db.Conn().Query(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	history := make([]model.Event, 0, 200)
	for rows.Next() {
		event := target
		var ns int64
		var responded *int64
		var timingJSON, quality string
		if err := rows.Scan(&event.AttemptID, &event.ProxyRequestID, &ns, &event.LatencyMS, &responded, &event.Tokens.Input, &event.Tokens.Output, &event.Tokens.Reasoning, &event.Tokens.Total, &quality, &timingJSON); err != nil {
			return nil, err
		}
		event.RequestedAt = time.Unix(0, ns).UTC()
		event.RespondedAt = nil
		if responded != nil {
			at := time.Unix(0, *responded).UTC()
			event.RespondedAt = &at
		}
		event.Tokens.Quality = model.TokenQuality(quality)
		event.Source = "native"
		event.ImportBatchID = ""
		event.Succeeded = true
		event.Outcome = "succeeded"
		event.OutputTiming = &model.OutputTiming{}
		if err := json.Unmarshal([]byte(timingJSON), event.OutputTiming); err != nil {
			continue
		}
		history = append(history, event)
	}
	return history, rows.Err()
}

// refreshOutputTiming reads the bounded page's optional evidence from the scalar
// column. Invalid optional JSON must not discard an otherwise valid accounting
// event. Event JSON is the temporary detail representation, not timing authority.
func (s *Store) refreshOutputTiming(ctx context.Context, events []model.Event) {
	if len(events) == 0 {
		return
	}
	start, end := events[0].RequestedAt, events[0].RequestedAt
	ids := make([]any, len(events))
	byID := make(map[string]int, len(events))
	for i, e := range events {
		if e.RequestedAt.Before(start) {
			start = e.RequestedAt
		}
		if e.RequestedAt.After(end) {
			end = e.RequestedAt
		}
		ids[i] = e.AttemptID
		byID[e.AttemptID] = i
	}
	if end.UnixNano() == 9223372036854775807 {
		return
	}
	q := model.Query{Start: start, End: end.Add(time.Nanosecond), TimeZone: "UTC"}
	selection, err := s.rawSelection(ctx, q, "attempt_id", "output_timing")
	if err != nil {
		return
	}
	rows, err := s.db.Conn().Query(ctx, "SELECT attempt_id,output_timing FROM ("+selection.SQL+") WHERE "+inClause("attempt_id", len(ids)), append(selection.Args, ids...)...)
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var raw *string
		if err := rows.Scan(&id, &raw); err != nil {
			return
		}
		i, ok := byID[id]
		if !ok {
			continue
		}
		events[i].OutputTiming = nil
		if raw != nil {
			var timing model.OutputTiming
			if err := json.Unmarshal([]byte(*raw), &timing); err == nil && timing.Validate() == nil {
				events[i].OutputTiming = &timing
			}
		}
	}
}
