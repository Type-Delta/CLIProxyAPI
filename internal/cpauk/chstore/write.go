package chstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func eventArguments(event model.Event, knownCost any, unpriced int64, ruleID, source string, batchID any, rate *model.NanoUSD, breakdown *model.PricingBreakdown) []any {
	var creditRate any
	if rate != nil {
		creditRate = int64(*rate)
	}
	sessionID := event.SessionID
	if sessionID != nil && event.SessionSource != nil {
		for _, source := range model.SessionSources {
			if source == *event.SessionSource {
				value := source + ":" + *sessionID
				sessionID = &value
				break
			}
		}
	}
	var stream any
	if event.Stream != nil {
		stream = *event.Stream
	}
	return []any{
		event.AttemptID, event.SchemaVersion, event.ProxyRequestID, string(event.RequestIDQuality), event.KeyID, event.RequestedAt.UnixNano(),
		event.Provider, event.ExecutorType, event.Model, nullStringPointer(event.RequestedAlias), event.EndpointClass,
		nullStringPointer(event.AuthType), nullStringPointer(event.CredentialID), nullStringPointer(event.CredentialIDAlgorithm),
		nullStringPointer(event.SessionDigest),
		event.Succeeded, nullIntPointer(event.UpstreamStatusCode), nullStringPointer(event.ErrorClass), event.LatencyMS,
		nullInt64Pointer(event.TimeToFirstTokenMS), nullStringPointer(event.ServiceTierRequested), nullStringPointer(event.ServiceTierUsed), event.Generated,
		event.Tokens.Input, event.Tokens.Output, event.Tokens.Reasoning, event.Tokens.Cached, event.Tokens.CacheRead,
		event.Tokens.CacheCreation, event.Tokens.Total, event.Tokens.Schema, string(event.Tokens.Quality), knownCost,
		unpriced, nullString(ruleID), nullString(source), batchID, nullInt64Pointer(event.GenerationTimeMS),
		nullStringPointer(event.ClientMethod), nullStringPointer(event.ClientPath), nullTimePointer(event.ReceivedAt),
		nullStringPointer(event.UpstreamMethod), nullStringPointer(event.UpstreamURL), nullTimePointer(event.UpstreamSentAt),
		nullRawJSONPointer(event.UpstreamUsageRaw), nullStringPointer(event.UpstreamErrorBody),
		nullIntPointer(event.ProxyStatusCode), nullStringPointer(event.ProxyError), nullTimePointer(event.RespondedAt), nullInt64Pointer(event.FirstTokenLatencyMS), nullInt64Pointer(event.ProviderLatencyMS), nullInt64Pointer(event.RoutingTimeMS), nullOutputTiming(event.OutputTiming), stream, nullStringPointer(event.ReasoningEffort), nullStringPointer(event.ProviderCredits), creditRate, nullPricingBreakdown(breakdown), nullStringPointer(sessionID), nullString(event.Outcome), nullStringPointer(event.FailureHop), nullStringPointer(event.CancellationSource), nullString(string(event.RoutingTrace)),
	}
}

func nullTimePointer(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return value.UnixNano()
}

func nullRawJSONPointer(value *model.RawJSON) any {
	if value == nil || *value == "" {
		return nil
	}
	return string(*value)
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullStringPointer(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullIntPointer(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullInt64Pointer(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullOutputTiming(value *model.OutputTiming) any {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func nullPricingBreakdown(value *model.PricingBreakdown) any {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

const eventColumns = ` attempt_id, schema_version, proxy_request_id, request_id_quality, key_id, requested_at_ns, provider, executor_type, model, requested_alias, endpoint_class, auth_type, credential_id, credential_id_algorithm, session_digest, succeeded, upstream_status_code, error_class, latency_ms, time_to_first_token_ms, service_tier_requested, service_tier_used, generated, input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens, accounting_schema, token_quality, known_cost_nano, unpriced_tokens, price_rule_id, price_source, import_batch_id, generation_time_ms, client_method, client_path, received_at_ns, upstream_method, upstream_url, upstream_sent_at_ns, upstream_usage_raw, upstream_error_body, proxy_status_code, proxy_error, responded_at_ns, first_token_latency_ms, provider_latency_ms, routing_time_ms, output_timing, stream, reasoning_effort, provider_credits, price_usd_per_credit_nano, pricing_breakdown, session_id, outcome, failure_hop, cancellation_source, routing_trace_json`

func (s *Store) WriteBatch(ctx context.Context, events []model.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	_, err := s.insertEvents(ctx, events, "", false)
	return err
}
func (s *Store) WriteImportBatch(ctx context.Context, events []model.Event, batchID string) (int64, error) {
	if batchID == "" {
		return 0, fmt.Errorf("import batch ID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return 0, ErrClosed
	}
	return s.insertEvents(ctx, events, batchID, false)
}

// insertEvents validates the complete batch before sending, and keeps the first
// occurrence of an attempt unless replace explicitly requests a new version.
func (s *Store) insertEvents(ctx context.Context, events []model.Event, batchID string, replace bool) (int64, error) {
	return s.insertEventsMode(ctx, events, batchID, replace, replace)
}

func (s *Store) insertEventsMode(ctx context.Context, events []model.Event, batchID string, replace, preserve bool) (int64, error) {
	if len(events) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(events))
	for i, event := range events {
		if err := event.Validate(); err != nil {
			return 0, fmt.Errorf("validate event %d: %w", i, err)
		}
		ids = append(ids, event.AttemptID)
	}
	// Recover staged retained imports before deduplication or raw cleanup.
	if !replace {
		if err := s.c4bRecoverLate(ctx); err != nil {
			return 0, err
		}
	}
	existing := make(map[string]bool)
	if !replace {
		for start := 0; start < len(ids); start += 1000 {
			end := min(start+1000, len(ids))
			rows, err := s.db.Conn().Query(ctx, "SELECT DISTINCT attempt_id FROM "+s.table("analytics_events")+" FINAL PREWHERE attempt_id IN (?) WHERE is_deleted=0 OR retained_generation>0 SETTINGS use_skip_indexes_if_final=1", ids[start:end])
			if err != nil {
				return 0, fmt.Errorf("inspect analytics attempt IDs: %w", err)
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					_ = rows.Close()
					return 0, err
				}
				existing[id] = true
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return 0, err
			}
		}
	}
	arguments := make([][]any, 0, len(events))
	var estimated int64
	for i, event := range events {
		if !replace && existing[event.AttemptID] {
			continue
		}
		existing[event.AttemptID] = true
		var priced aggregate.PriceResult
		if preserve {
			priced = aggregate.PriceResult{KnownCost: event.KnownCost, UnpricedTokens: event.UnpricedTokens, RuleID: event.PriceRuleID, Source: event.PriceSource, PriceUSDPerCredit: event.PriceUSDPerCredit, Breakdown: event.PricingBreakdown}
		} else {
			var recorded bool
			if batchID != "" {
				priced, recorded = aggregate.RecordedPrice(event)
			}
			if !recorded {
				var err error
				priced, err = s.config.PriceBook.Price(event)
				if err != nil {
					return 0, fmt.Errorf("price event %d: %w", i, err)
				}
			}
		}
		event.KnownCost = priced.KnownCost
		event.UnpricedTokens = priced.UnpricedTokens
		event.PriceRuleID = priced.RuleID
		event.PriceSource = priced.Source
		event.PriceUSDPerCredit = priced.PriceUSDPerCredit
		event.PricingBreakdown = priced.Breakdown
		if batchID != "" {
			event.ImportBatchID = batchID
		} else if !preserve {
			event.ImportBatchID = ""
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			return 0, err
		}
		estimated += int64(max(1024, len(encoded)))
		var known any
		if priced.KnownCost != nil {
			known = int64(*priced.KnownCost)
		}
		args := eventArguments(event, known, priced.UnpricedTokens, priced.RuleID, priced.Source, nullString(event.ImportBatchID), priced.PriceUSDPerCredit, priced.Breakdown)
		arguments = append(arguments, append(args, string(encoded), s.nextVersion(), uint8(0)))
	}
	if len(arguments) == 0 {
		return 0, nil
	}
	if err := s.checkQuota(ctx, estimated); err != nil {
		return 0, err
	}
	defer s.invalidateEventHistory(events)
	// Below-floor writes must publish retained snapshots, never hidden raw rows.
	if !replace && s.c4bLateArguments(arguments) {
		return s.c4bInsertLate(ctx, arguments, estimated)
	}
	batch, err := s.db.Conn().PrepareBatch(synchronousWriteContext(ctx), "INSERT INTO "+s.table("analytics_events")+" ("+eventColumns+",event_json,version,is_deleted)")
	if err != nil {
		return 0, fmt.Errorf("prepare analytics events: %w", err)
	}
	defer func() { _ = batch.Abort() }()
	for _, args := range arguments {
		if err := batch.Append(args...); err != nil {
			return 0, fmt.Errorf("append analytics event: %w", err)
		}
	}
	if err := batch.Send(); err != nil {
		return 0, fmt.Errorf("send analytics events: %w", err)
	}
	s.reserveBudget(estimated)
	return int64(len(arguments)), nil
}

func (s *Store) WriteBatchWithPatches(ctx context.Context, events []model.Event, patches []model.ProxyResponsePatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	if _, err := s.insertEvents(ctx, events, "", false); err != nil {
		return err
	}
	valid := make([]model.ProxyResponsePatch, 0, len(patches))
	ids := make([]string, 0, len(patches))
	for _, patch := range patches {
		if !model.IsCorrelationID(patch.ProxyRequestID) || patch.StatusCode < 100 || patch.StatusCode > 599 {
			continue
		}
		valid = append(valid, patch)
		ids = append(ids, patch.ProxyRequestID)
	}
	if len(valid) == 0 {
		return nil
	}
	// Read each existing outcome before choosing the eligible attempts. The write
	// lock also serializes patches arriving in separate collector batches.
	rows, err := s.db.Conn().Query(ctx, "SELECT attempt_id,proxy_request_id,requested_at_ns FROM "+s.table("analytics_events")+" FINAL PREWHERE proxy_request_id IN (?) WHERE is_deleted=0 AND proxy_status_code IS NULL AND responded_at_ns IS NULL SETTINGS use_skip_indexes_if_final=1", ids)
	if err != nil {
		return err
	}
	eligible := make(map[string][]string)
	var affectedStart, affectedEnd time.Time
	for rows.Next() {
		var attempt, request string
		var requestedNS int64
		if err := rows.Scan(&attempt, &request, &requestedNS); err != nil {
			_ = rows.Close()
			return err
		}
		eligible[request] = append(eligible[request], attempt)
		requestedAt := time.Unix(0, requestedNS).UTC()
		if affectedStart.IsZero() || requestedAt.Before(affectedStart) {
			affectedStart = requestedAt
		}
		if affectedEnd.IsZero() || requestedAt.After(affectedEnd) {
			affectedEnd = requestedAt
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	// An existing outcome makes its attempts ineligible regardless of which
	// sequence won. Fetch only references; the event payload is unnecessary.
	patched := make(map[string]bool)
	for start := 0; start < len(ids); start += 1000 {
		rows, err := s.db.Conn().Query(ctx, "SELECT attempt_ids FROM "+s.table("analytics_outcomes")+" WHERE proxy_request_id IN (?)", ids[start:min(start+1000, len(ids))])
		if err != nil {
			return err
		}
		for rows.Next() {
			var attempts []string
			if err := rows.Scan(&attempts); err != nil {
				_ = rows.Close()
				return err
			}
			for _, attempt := range attempts {
				patched[attempt] = true
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
	}
	for request, attempts := range eligible {
		remaining := attempts[:0]
		for _, attempt := range attempts {
			if !patched[attempt] {
				remaining = append(remaining, attempt)
			}
		}
		eligible[request] = remaining
	}
	var args [][]any
	var estimated int64
	for _, patch := range valid {
		attempts := eligible[patch.ProxyRequestID]
		if len(attempts) == 0 {
			continue
		}
		delete(eligible, patch.ProxyRequestID)
		at := patch.RespondedAt
		if at.IsZero() {
			at = time.Now().UTC()
		}
		args = append(args, []any{patch.ProxyRequestID, attempts, int64(patch.StatusCode), patch.Error, at.UnixNano(), s.nextVersion()})
		estimated += int64(128 + len(attempts)*32 + len(patch.Error))
	}
	if len(args) == 0 {
		return nil
	}
	if err := s.checkQuota(ctx, estimated); err != nil {
		return err
	}
	defer func() {
		if !affectedStart.IsZero() {
			s.invalidateHistory(affectedStart, affectedEnd.Add(time.Nanosecond))
		}
	}()
	batch, err := s.db.Conn().PrepareBatch(synchronousWriteContext(ctx), "INSERT INTO "+s.table("analytics_outcomes")+" (proxy_request_id,attempt_ids,status_code,error,responded_at_ns,sequence)")
	if err != nil {
		return err
	}
	defer func() { _ = batch.Abort() }()
	for _, values := range args {
		if err := batch.Append(values...); err != nil {
			return err
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("send analytics response patches: %w", err)
	}
	s.reserveBudget(estimated)
	return nil
}

// loadEvents restores lossless Event JSON and applies the earliest eligible patch.
// Callers hold the store lock. Only trusted scalar SQL belongs in where.
func (s *Store) loadEvents(ctx context.Context, where string, args ...any) ([]model.Event, error) {
	statement := "SELECT event_json FROM " + s.table("analytics_events") + " FINAL WHERE is_deleted=0"
	if strings.TrimSpace(where) != "" {
		statement += " AND (" + where + ")"
	}
	rows, err := s.db.Conn().Query(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	events := make([]model.Event, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var event model.Event
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("decode stored analytics event: %w", err)
		}
		events = append(events, event)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	type outcome struct {
		status   int
		err      string
		at       int64
		sequence uint64
	}
	outcomes := make(map[string]outcome)
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ProxyRequestID)
	}
	for start := 0; start < len(ids); start += 1000 {
		rows, err := s.db.Conn().Query(ctx, "SELECT attempt_ids,status_code,error,responded_at_ns,sequence FROM "+s.table("analytics_outcomes")+" WHERE proxy_request_id IN (?)", ids[start:min(start+1000, len(ids))])
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var attempts []string
			var status, at int64
			var message string
			var sequence uint64
			if err := rows.Scan(&attempts, &status, &message, &at, &sequence); err != nil {
				_ = rows.Close()
				return nil, err
			}
			for _, id := range attempts {
				old, ok := outcomes[id]
				if !ok || sequence < old.sequence {
					outcomes[id] = outcome{int(status), message, at, sequence}
				}
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	for i := range events {
		event := &events[i]
		if event.ProxyStatusCode != nil || event.RespondedAt != nil {
			continue
		}
		if patch, ok := outcomes[event.AttemptID]; ok {
			status := patch.status
			event.ProxyStatusCode = &status
			if patch.err != "" {
				message := patch.err
				event.ProxyError = &message
			} else {
				event.ProxyError = nil
			}
			at := time.Unix(0, patch.at).UTC()
			event.RespondedAt = &at
		}
	}
	return events, nil
}
