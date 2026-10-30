package chstore

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// enrichArrivalEstimates consumes at most 200 unique labels per cohort. SQL
// applies the SQLite index bounds before ordering, deduplicating and limiting.
// Optional history failures never fail an accounting read. Caller holds s.mu.
func (s *Store) enrichArrivalEstimates(ctx context.Context, events []model.Event) {
	if _, ok := ctx.Value(readCutoffsContextKey{}).(*readCutoffsSnapshot); !ok {
		ctx = withReadCutoffs(ctx)
	}
	s.refreshOutputTiming(ctx, events)
	targets := make([]model.Event, 0, len(events))
	indices := make([]int, 0, len(events))
	for i := range events {
		event := &events[i]
		event.EnrichThroughput()
		event.ThroughputEstimate = nil
		estimate := model.EstimateArrivalThroughput(*event, nil)
		if estimate.Reason == "observed_available" {
			continue
		}
		event.ThroughputEstimate = &estimate
		if estimate.Reason == "insufficient_history" {
			targets = append(targets, *event)
			indices = append(indices, i)
		}
	}
	histories, err := s.arrivalEstimateHistories(ctx, targets)
	for i, index := range indices {
		if err != nil {
			events[index].ThroughputEstimate.Reason = "history_unavailable"
			continue
		}
		estimate := model.EstimateArrivalThroughput(events[index], histories[i])
		events[index].ThroughputEstimate = &estimate
	}
}

func (s *Store) arrivalEstimateHistory(ctx context.Context, target model.Event) ([]model.Event, error) {
	histories, err := s.arrivalEstimateHistories(ctx, []model.Event{target})
	if err != nil {
		return nil, err
	}
	return histories[0], nil
}

func (s *Store) arrivalEstimateHistorySelection(ctx context.Context, targets []model.Event) (readSelection, *ext.Table, error) {
	start, end := targets[0].RequestedAt.Add(-24*time.Hour), targets[0].RequestedAt
	for _, target := range targets[1:] {
		if at := target.RequestedAt.Add(-24 * time.Hour); at.Before(start) {
			start = at
		}
		if target.RequestedAt.After(end) {
			end = target.RequestedAt
		}
	}
	q := model.Query{Start: start, End: end, TimeZone: "UTC"}
	columns := []string{"attempt_id", "proxy_request_id", "requested_at_ns", "latency_ms", "responded_at_ns", "input_tokens", "output_tokens", "reasoning_tokens", "total_tokens", "token_quality", "output_timing", "succeeded", "outcome", "import_batch_id", "accounting_schema", "key_id", "provider", "model", "executor_type", "credential_id", "credential_id_algorithm", "stream", "reasoning_effort", "service_tier_requested", "service_tier_used"}
	selection, err := s.rawSelection(ctx, q, columns...)
	if err != nil {
		return readSelection{}, nil, err
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
		"key_id=target_key", "provider=target_provider", "model=target_model", "executor_type=target_executor", "isNotDistinctFrom(credential_id,target_credential)", "isNotDistinctFrom(credential_id_algorithm,target_credential_algorithm)", js("protocol") + "=target_protocol", js("source") + "=target_source", js("coverage") + "=target_coverage",
		"isNotDistinctFrom(stream,target_stream)", "isNotDistinctFrom(reasoning_effort,target_reasoning)", "isNotDistinctFrom(service_tier_requested,target_tier_requested)", "isNotDistinctFrom(service_tier_used,target_tier_used)",
		"requested_at_ns>=target_start", "requested_at_ns<target_end", completion + ">=target_start", completion + "<target_end", "proxy_request_id<>target_request", "proxy_request_id<>''",
	}
	targetTable, err := ext.NewTable("arrival_targets",
		ext.Column("target_index", "UInt64"), ext.Column("target_key", "String"), ext.Column("target_provider", "String"), ext.Column("target_model", "String"), ext.Column("target_executor", "String"), ext.Column("target_credential", "Nullable(String)"), ext.Column("target_credential_algorithm", "Nullable(String)"), ext.Column("target_protocol", "String"), ext.Column("target_source", "String"), ext.Column("target_coverage", "String"), ext.Column("target_stream", "Nullable(Bool)"), ext.Column("target_reasoning", "Nullable(String)"), ext.Column("target_tier_requested", "Nullable(String)"), ext.Column("target_tier_used", "Nullable(String)"), ext.Column("target_start", "Int64"), ext.Column("target_end", "Int64"), ext.Column("target_request", "String"))
	if err != nil {
		return readSelection{}, nil, err
	}
	for i, target := range targets {
		timing := target.OutputTiming
		if err := targetTable.Append(uint64(i), target.KeyID, target.Provider, target.Model, target.ExecutorType, target.CredentialID, target.CredentialIDAlgorithm, timing.Protocol, timing.Source, timing.Coverage, target.Stream, target.ReasoningEffort, target.ServiceTierRequested, target.ServiceTierUsed, target.RequestedAt.Add(-24*time.Hour).UnixNano(), target.RequestedAt.UnixNano(), target.ProxyRequestID); err != nil {
			return readSelection{}, nil, err
		}
	}
	fields := "target_index,attempt_id,proxy_request_id,requested_at_ns,latency_ms,responded_at_ns,input_tokens,output_tokens,reasoning_tokens,total_tokens,token_quality,output_timing"
	order := " ORDER BY target_index,completed_at DESC,proxy_request_id ASC,attempt_id ASC"
	statement := "SELECT " + fields + " FROM (SELECT " + fields + "," + completion + " AS completed_at FROM (" + selection.SQL + ") AS history CROSS JOIN arrival_targets AS targets WHERE " + strings.Join(predicates, " AND ") + order + " LIMIT 1 BY target_index,proxy_request_id)" + order + " LIMIT 200 BY target_index"

	return readSelection{statement, selection.Args}, targetTable, nil
}

// The event scan is shared by all targets. Per-target bounds and limits preserve
// causal completion, first request identity, and the independent label budget.
func (s *Store) arrivalEstimateHistories(ctx context.Context, targets []model.Event) ([][]model.Event, error) {
	histories := make([][]model.Event, len(targets))
	if len(targets) == 0 {
		return histories, nil
	}
	if _, ok := ctx.Value(readCutoffsContextKey{}).(*readCutoffsSnapshot); !ok {
		ctx = withReadCutoffs(ctx)
	}
	selection, targetsTable, err := s.arrivalEstimateHistorySelection(ctx, targets)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Conn().Query(ch.Context(ctx, ch.WithExternalTable(targetsTable)), selection.SQL, selection.Args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var index uint64
		var event model.Event
		var ns int64
		var responded *int64
		var timingJSON, quality string
		if err := rows.Scan(&index, &event.AttemptID, &event.ProxyRequestID, &ns, &event.LatencyMS, &responded, &event.Tokens.Input, &event.Tokens.Output, &event.Tokens.Reasoning, &event.Tokens.Total, &quality, &timingJSON); err != nil {
			return nil, err
		}
		decoded := event
		event = targets[index]
		event.AttemptID, event.ProxyRequestID, event.LatencyMS = decoded.AttemptID, decoded.ProxyRequestID, decoded.LatencyMS
		event.Tokens.Input, event.Tokens.Output, event.Tokens.Reasoning, event.Tokens.Total = decoded.Tokens.Input, decoded.Tokens.Output, decoded.Tokens.Reasoning, decoded.Tokens.Total
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
		histories[index] = append(histories[index], event)
	}
	return histories, rows.Err()
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
