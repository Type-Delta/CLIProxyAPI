package chstore

// Frozen pre-optimization readers provide an independent parity reference.
import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"slices"
	"strconv"
	"strings"
	"time"
)

func (s *Store) s2LegacyRawSelection(ctx context.Context, q model.Query, columns ...string) (readSelection, error) {
	bounds, err := s.s2LegacyReadCutoffs(ctx)
	if err != nil {
		return readSelection{}, err
	}
	if bounds.rawSet && q.Start.UnixNano() < bounds.raw {
		q.Start = time.Unix(0, bounds.raw).UTC()
	}
	where, args, err := buildWhere(q)
	if err != nil {
		return readSelection{}, err
	}
	baseQuery := q
	baseQuery.Filters = make(map[string]json.RawMessage, len(q.Filters))
	for name, value := range q.Filters {
		if name != "outcome" && name != "result" && name != "success" {
			baseQuery.Filters[name] = value
		}
	}
	baseWhere, baseArgs, err := buildWhere(baseQuery)
	if err != nil {
		return readSelection{}, err
	}
	// Include only projection and predicate dependencies. ClickHouse prunes unused
	// dependencies further when the enclosing reader aggregates a subset.
	needed := make(map[string]bool, len(columns)+10)
	for _, c := range columns {
		needed[c] = true
	}
	needed["requested_at_ns"] = true
	if len(q.KeyIDs) > 0 {
		needed["key_id"] = true
	}
	filterColumns := map[string][]string{
		"provider":       {"provider"},
		"model":          {"model"},
		"credential_id":  {"credential_id"},
		"endpoint_class": {"endpoint_class"},
		"auth_type":      {"auth_type"},
		"service_tier":   {"service_tier_used", "service_tier_requested"},
		"error_class":    {"error_class"},
		"status_code":    {"upstream_status_code"},
		"token_quality":  {"token_quality"},
		"session_id":     {"session_id"},
		"source":         {"import_batch_id"},
		"generated":      {"generated"},
		"success":        {"succeeded", "outcome", "upstream_error_body"},
		"result":         {"succeeded", "outcome", "upstream_error_body"},
		"outcome":        {"outcome", "succeeded", "upstream_error_body"},
	}
	for name := range q.Filters {
		for _, c := range filterColumns[name] {
			needed[c] = true
		}
	}
	if needed["session_captured"] {
		delete(needed, "session_captured")
		for _, c := range []string{"session_id", "outcome", "import_batch_id"} {
			needed[c] = true
		}
	}
	overlay := needed["proxy_status_code"] || needed["proxy_error"] || needed["responded_at_ns"]
	if overlay {
		for _, c := range []string{"attempt_id", "proxy_status_code", "responded_at_ns"} {
			needed[c] = true
		}
	}
	rawColumns := make([]string, 0, len(needed))
	inner := make([]string, 0, len(needed))
	for c := range needed {
		rawColumns = append(rawColumns, c)
		switch c {
		case "proxy_status_code":
			inner = append(inner, "if(e.proxy_status_code IS NULL AND e.responded_at_ns IS NULL AND o.present=1, tupleElement(o.patch,1),e.proxy_status_code) AS proxy_status_code")
		case "proxy_error":
			inner = append(inner, "if(e.proxy_status_code IS NULL AND e.responded_at_ns IS NULL AND o.present=1, nullIf(tupleElement(o.patch,2),''),e.proxy_error) AS proxy_error")
		case "responded_at_ns":
			inner = append(inner, "if(e.proxy_status_code IS NULL AND e.responded_at_ns IS NULL AND o.present=1, tupleElement(o.patch,3),e.responded_at_ns) AS responded_at_ns")
		default:
			inner = append(inner, "e."+c+" AS "+c)
		}
	}
	slices.Sort(inner)
	slices.Sort(rawColumns)
	projection := append([]string(nil), columns...)
	for i, c := range projection {
		if c == "session_captured" {
			// An absent key is zero. Native ClickHouse capture has no migration boundary.
			projection[i] = "(session_id IS NOT NULL OR outcome IS NOT NULL OR (import_batch_id IS NULL AND requested_at_ns >= (SELECT coalesce(max(toInt64(value)),0) FROM " + s.table("analytics_metadata") + " FINAL WHERE key='session_capture_since_ns' AND is_deleted=0))) AS session_captured"
		}
	}
	source := "(SELECT " + strings.Join(rawColumns, ",") + " FROM " + s.table("analytics_events") + " FINAL " + baseWhere + " AND is_deleted=0) AS e"
	if overlay {
		source += " LEFT JOIN (SELECT attempt_id, argMin(tuple(status_code,error,responded_at_ns),sequence) AS patch, toUInt8(1) AS present FROM " + s.table("analytics_outcomes") + " ARRAY JOIN attempt_ids AS attempt_id GROUP BY attempt_id) AS o ON e.attempt_id=o.attempt_id"
	}
	statement := "SELECT " + strings.Join(projection, ",") + " FROM (SELECT " + strings.Join(inner, ",") + " FROM " + source + ") " + where
	return readSelection{statement, append(baseArgs, args...)}, nil
}

func (s *Store) s2LegacyReadCutoffs(ctx context.Context) (readCutoffs, error) {
	var result readCutoffs
	if !s.retentionCutoff.IsZero() {
		result.raw = s.retentionCutoff.UnixNano()
		result.rawSet = true
	}
	for _, key := range []string{"retention_raw_cutoff_ns", "retention_raw_checkpoint_ns", "retention_hourly_cutoff_ns"} {
		value, ok, err := s.metadata(ctx, key)
		if err != nil {
			return result, err
		}
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return result, fmt.Errorf("read %s: %w", key, err)
		}
		if key == "retention_hourly_cutoff_ns" {
			result.hourly = n
			result.hourlySet = true
		} else {
			if key == "retention_raw_checkpoint_ns" {
				n++
			}
			if !result.rawSet || n > result.raw {
				result.raw = n
			}
			result.rawSet = true
		}
	}
	return result, nil
}

// retainedSelection reads only published history. The same predicate is used
// for aggregate facts and exact request identities. No physical-row inference.
func (s *Store) s2LegacyRetainedSelection(ctx context.Context, q model.Query, requests bool, columns ...string) (readSelection, error) {
	where, args, err := s2LegacyBuildRollupWhere(q, "bucket_start_ns", "bucket_end_ns")
	if err != nil {
		return readSelection{}, err
	}
	bounds, err := s.s2LegacyReadCutoffs(ctx)
	if err != nil {
		return readSelection{}, err
	}
	table := "analytics_rollups"
	if requests {
		table = "analytics_request_rollups"
	}
	// Both retained tables must resolve the same published bucket generation.
	where += " AND is_deleted=0 AND " + s.c4bPublishedPredicate()
	if !bounds.rawSet {
		where += " AND 0"
	} else {
		where += " AND bucket_end_ns <= ? AND ((grain='hourly' AND (?=0 OR bucket_start_ns >= ?)) OR (grain='daily' AND ?=1 AND bucket_end_ns <= ?))"
		args = append(args, bounds.raw, bounds.hourlySet, bounds.hourly, bounds.hourlySet, bounds.hourly)
	}
	return readSelection{"SELECT " + strings.Join(columns, ",") + " FROM " + s.table(table) + " FINAL " + where, args}, nil
}

// validateRetainedRange mirrors SQLite's selection-dependent partial-bucket and
// timezone errors, but ignores unpublished destination buckets.
func (s *Store) s2LegacyValidateRetainedRange(ctx context.Context, q model.Query) error {
	selectionQ := q
	selectionQ.Start = time.Unix(0, -9223372036854775808).UTC()
	selectionQ.End = time.Unix(0, 9223372036854775807).UTC()
	selected, err := s.s2LegacyRetainedSelection(ctx, selectionQ, false, "bucket_start_ns", "bucket_end_ns", "grain")
	if err != nil {
		return err
	}
	var overlapping, partial uint64
	args := append([]any{q.Start.UnixNano(), q.End.UnixNano()}, selected.Args...)
	args = append(args, q.End.UnixNano(), q.Start.UnixNano())
	err = s.db.Conn().QueryRow(ctx, "SELECT count(),countIf(NOT (bucket_start_ns>=? AND bucket_end_ns<=?)) FROM ("+selected.SQL+") WHERE bucket_start_ns<? AND bucket_end_ns>?", args...).Scan(&overlapping, &partial)
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
	if overlapping > 0 && q.Operation == model.OperationTimeseries {
		rows, err := s.db.Conn().Query(ctx, "SELECT DISTINCT grain FROM ("+selected.SQL+") WHERE bucket_start_ns<? AND bucket_end_ns>? ORDER BY grain", append(selected.Args, q.End.UnixNano(), q.Start.UnixNano())...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var grain string
			if err := rows.Scan(&grain); err != nil {
				return err
			}
			if err := validateRollupWidth(grain, q.BucketWidth); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	return nil
}

func s2LegacyBuildRollupWhere(query model.Query, startColumn, endColumn string) (string, []any, error) {
	// Keep the grain predicate explicit so SQLite can use the composite
	// (grain,bucket_start_ns) range index instead of scanning both prefixes.
	clauses := []string{startColumn + " >= ?", endColumn + " <= ?", "grain IN ('hourly','daily')"}
	arguments := []any{query.Start.UnixNano(), query.End.UnixNano()}
	if len(query.KeyIDs) != 0 {
		clauses = append(clauses, inClause("key_id", len(query.KeyIDs)))
		for _, keyID := range query.KeyIDs {
			arguments = append(arguments, keyID)
		}
	}
	columns := map[string]string{
		"provider": "provider", "model": "model", "credential_id": "credential_id",
		"endpoint_class": "endpoint_class", "auth_type": "auth_type", "service_tier": "service_tier",
		"error_class": "error_class", "status_code": "status_code", "token_quality": "token_quality",
	}
	for _, name := range sortedFilterNames(query.Filters) {
		raw := query.Filters[name]
		if name == "generated" {
			return "", nil, fmt.Errorf("generated filtering is unavailable after raw-event retention")
		}
		if name == "success" {
			var value bool
			if err := json.Unmarshal(raw, &value); err != nil {
				return "", nil, fmt.Errorf("decode success filter: %w", err)
			}
			clauses = append(clauses, "succeeded = ?")
			arguments = append(arguments, value)
			continue
		}
		column, ok := columns[name]
		if !ok {
			return "", nil, fmt.Errorf("unsupported retained store filter %q", name)
		}
		var values []any
		if name == "status_code" {
			var parsed []int
			if err := json.Unmarshal(raw, &parsed); err != nil {
				return "", nil, fmt.Errorf("decode status filter: %w", err)
			}
			for _, value := range parsed {
				values = append(values, value)
			}
		} else {
			var parsed []string
			if err := json.Unmarshal(raw, &parsed); err != nil {
				return "", nil, fmt.Errorf("decode %s filter: %w", name, err)
			}
			for _, value := range parsed {
				values = append(values, value)
			}
		}
		clauses = append(clauses, inClause(column, len(values)))
		arguments = append(arguments, values...)
	}
	return "WHERE " + strings.Join(clauses, " AND "), arguments, nil
}

func (s *Store) s2LegacyArrivalEstimateHistory(ctx context.Context, target model.Event) ([]model.Event, error) {
	start := target.RequestedAt.Add(-24 * time.Hour)
	q := model.Query{Start: start, End: target.RequestedAt, TimeZone: "UTC"}
	columns := []string{"attempt_id", "proxy_request_id", "requested_at_ns", "latency_ms", "responded_at_ns", "input_tokens", "output_tokens", "reasoning_tokens", "total_tokens", "token_quality", "output_timing", "succeeded", "outcome", "import_batch_id", "accounting_schema", "key_id", "provider", "model", "executor_type", "credential_id", "credential_id_algorithm", "stream", "reasoning_effort", "service_tier_requested", "service_tier_used"}
	selection, err := s.s2LegacyRawSelection(ctx, q, columns...)
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
func (s *Store) s2LegacyModelLeaderboard(ctx context.Context, query model.Query) (model.ModelLeaderboard, error) {
	if err := s.validateQuery(&query, model.OperationModelLeaderboard); err != nil {
		return model.ModelLeaderboard{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.ModelLeaderboard{}, ErrClosed
	}
	if !s.retentionCutoff.IsZero() && query.Start.Before(s.retentionCutoff) {
		return model.ModelLeaderboard{}, RetainedRangeError{Cutoff: s.retentionCutoff}
	}
	raw, err := s.s2LegacyRawSelection(ctx, query, "model", "provider", "succeeded", "session_digest", "output_tokens", "reasoning_tokens", "accounting_schema", "output_timing", "outcome")
	if err != nil {
		return model.ModelLeaderboard{}, err
	}
	statement := `WITH evidence AS (SELECT *, ` + c2r2SpeedSQL() + ` AS speed,
 if(accounting_schema='normalized-v1',output_tokens,output_tokens+reasoning_tokens) AS generated,
 succeeded=1 AND coalesce(outcome,'')!='canceled' AS eligible FROM (` + raw.SQL + `) WHERE model!=''),
 stats AS (SELECT model, toInt64(count()) AS attempts, toInt64(countIf(succeeded=0)) AS failed,
 toInt64(uniqExact(provider)) AS providers, toInt64(uniqExactIf(session_digest,coalesce(session_digest,'')!='')) AS sessions,
 arraySort(groupArrayIf(assumeNotNull(speed),eligible AND speed IS NOT NULL)) AS speeds,
 arraySort(groupArrayIf(generated,eligible AND accounting_schema!='raw-fallback-v1' AND generated>0)) AS talkative,
 sumIf(reasoning_tokens,eligible AND accounting_schema!='raw-fallback-v1' AND generated>0) AS reasoning,
 sumIf(generated,eligible AND accounting_schema!='raw-fallback-v1' AND generated>0) AS generated_total FROM evidence GROUP BY model),
 awards AS (
 SELECT 'most_used' AS metric,model,toFloat64(sessions) AS value,sessions AS tie,providers,CAST(NULL AS Nullable(Int64)) AS attempts,CAST(NULL AS Nullable(Int64)) AS failed,CAST(NULL AS Nullable(Int64)) AS samples,CAST(NULL AS Nullable(Float64)) AS share FROM stats WHERE sessions>0
 UNION ALL SELECT 'most_reliable',model,toFloat64(attempts-failed)/attempts,attempts,NULL,attempts,failed,NULL,NULL FROM stats WHERE attempts>=200
 UNION ALL SELECT 'speed',model,arrayElement(speeds,toUInt64(ceil(length(speeds)*0.70))),toInt64(length(speeds)),NULL,attempts-failed,NULL,toInt64(length(speeds)),NULL FROM stats WHERE length(speeds)>0 AND arrayElement(speeds,toUInt64(ceil(length(speeds)*0.70)))>=80
 UNION ALL SELECT 'most_talkative',model,toFloat64(arrayElement(talkative,toUInt64(ceil(length(talkative)*0.95)))),toInt64(length(talkative)),NULL,NULL,NULL,toInt64(length(talkative)),least(1.,toFloat64(reasoning)/generated_total) FROM stats WHERE length(talkative)>0
 ) SELECT metric,model,value,providers,attempts,failed,samples,share FROM awards ORDER BY metric,value DESC,tie DESC,model LIMIT 6 BY metric`
	rows, err := s.db.Conn().Query(ctx, statement, raw.Args...)
	if err != nil {
		return model.ModelLeaderboard{}, fmt.Errorf("query model leaderboard: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := model.ModelLeaderboard{Start: query.Start, End: query.End, Awards: []model.ModelLeaderboardAward{}}
	indices := map[string]int{}
	for _, metric := range []string{"most_used", "most_reliable", "speed", "most_talkative"} {
		indices[metric] = len(result.Awards)
		result.Awards = append(result.Awards, model.ModelLeaderboardAward{Metric: metric, Rows: []model.ModelLeaderboardRow{}})
	}
	for rows.Next() {
		var metric string
		var row model.ModelLeaderboardRow
		if err := rows.Scan(&metric, &row.Model, &row.Value, &row.Providers, &row.Attempts, &row.Failed, &row.Samples, &row.ReasoningShare); err != nil {
			return model.ModelLeaderboard{}, err
		}
		award := &result.Awards[indices[metric]]
		row.Rank = len(award.Rows) + 1
		award.Rows = append(award.Rows, row)
	}
	return result, rows.Err()
}

// c2r2SpeedSQL mirrors OutputTiming.Validate and ArrivalThroughput for observed
// windows. Unavailable timing and legacy generation_time_ms never produce TPS.

func (s *Store) s2LegacyFactsSelection(ctx context.Context, q model.Query) (readSelection, error) {
	columns := []string{"requested_at_ns", "succeeded", "input_tokens", "output_tokens", "reasoning_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens", "total_tokens", "token_quality", "known_cost_nano", "unpriced_tokens", "generation_time_ms", "outcome"}
	raw, err := s.s2LegacyRawSelection(ctx, q, columns...)
	if err != nil {
		return readSelection{}, err
	}
	fields := "succeeded,input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,token_quality,known_cost_nano,unpriced_tokens"
	retained, err := s.s2LegacyRetainedSelection(ctx, q, false, "bucket_start_ns", "upstream_attempts", fields)
	if err != nil {
		return readSelection{}, err
	}
	return readSelection{"SELECT requested_at_ns AS at_ns,toInt64(1) AS upstream_attempts," + fields + ",if(outcome='canceled',NULL,generation_time_ms) AS generation_time_ms FROM (" + raw.SQL + ") UNION ALL SELECT bucket_start_ns AS at_ns,upstream_attempts," + fields + ",CAST(NULL AS Nullable(Int64)) AS generation_time_ms FROM (" + retained.SQL + ")", append(raw.Args, retained.Args...)}, nil
}

func (s *Store) s2LegacyCombinedProxyRequests(ctx context.Context, q model.Query) (int64, error) {
	raw, err := s.s2LegacyRawSelection(ctx, q, "proxy_request_id")
	if err != nil {
		return 0, err
	}
	retained, err := s.s2LegacyRetainedSelection(ctx, q, true, "proxy_request_id")
	if err != nil {
		return 0, err
	}
	var count uint64
	err = s.db.Conn().QueryRow(ctx, "SELECT uniqExact(proxy_request_id) FROM ("+raw.SQL+" UNION ALL "+retained.SQL+")", append(raw.Args, retained.Args...)...).Scan(&count)
	return int64(count), err
}

func (s *Store) s2LegacySummary(ctx context.Context, query model.Query) (model.Summary, error) {
	if err := s.validateQuery(&query, model.OperationSummary); err != nil {
		return model.Summary{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.Summary{}, ErrClosed
	}
	if err := s.s2LegacyValidateRetainedRange(ctx, query); err != nil {
		return model.Summary{}, err
	}
	facts, err := s.s2LegacyFactsSelection(ctx, query)
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
	result.proxyRequests, err = s.s2LegacyCombinedProxyRequests(ctx, query)
	if err != nil {
		return model.Summary{}, err
	}
	timings, err := s.s2LegacyTimingMetrics(ctx, query, false)
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

func (s *Store) s2LegacyEvents(ctx context.Context, query model.Query, includeTrace bool) (model.EventPage, error) {
	if err := s.validateQuery(&query, model.OperationEvents); err != nil {
		return model.EventPage{}, err
	}
	selection, err := query.SelectionDigest()
	if err != nil {
		return model.EventPage{}, err
	}
	var cursor model.Cursor
	if query.Cursor != "" {
		cursor, err = s.config.CursorCodec.Decode(query.Cursor)
		if err != nil {
			return model.EventPage{}, err
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.EventPage{}, ErrClosed
	}
	bounds, err := s.s2LegacyReadCutoffs(ctx)
	if err != nil {
		return model.EventPage{}, err
	}
	if bounds.rawSet && query.Start.UnixNano() < bounds.raw {
		return model.EventPage{}, RetainedRangeError{Cutoff: time.Unix(0, bounds.raw).UTC()}
	}
	selectionSQL, err := s.s2LegacyRawSelection(ctx, query, "event_json", "proxy_status_code", "proxy_error", "responded_at_ns", "session_captured", "session_id", "requested_at_ns", "attempt_id")
	if err != nil {
		return model.EventPage{}, err
	}
	countSQL, err := s.s2LegacyRawSelection(ctx, query, "attempt_id")
	if err != nil {
		return model.EventPage{}, err
	}
	var count uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM ("+countSQL.SQL+")", countSQL.Args...).Scan(&count); err != nil {
		return model.EventPage{}, err
	}
	// Keep the page projection in the same foundation, without ordering-only columns.
	pageSQL := selectionSQL
	// Replace the trace in SQL so list pages never transfer or decode its payload.
	eventJSON := "JSONMergePatch(event_json,?)"
	if includeTrace {
		eventJSON = "event_json"
	} else {
		pageSQL.Args = append([]any{`{"routing_trace":null}`}, pageSQL.Args...)
	}
	pageSQL.SQL = "SELECT " + eventJSON + ",proxy_status_code,proxy_error,responded_at_ns,session_captured,session_id FROM (" + selectionSQL.SQL + ")"
	suffix := ""
	var pageArgs []any
	if cursor.RequestedAt != nil {
		suffix = " WHERE requested_at_ns < ? OR (requested_at_ns = ? AND attempt_id > ?)"
		pageArgs = append(pageArgs, cursor.RequestedAt.UnixNano(), cursor.RequestedAt.UnixNano(), cursor.AttemptID)
	}
	suffix += " ORDER BY requested_at_ns DESC,attempt_id ASC LIMIT ?"
	pageArgs = append(pageArgs, query.PageSize+1)
	events, err := s.selectedEventPage(ctx, pageSQL, suffix, pageArgs...)
	if err != nil {
		return model.EventPage{}, err
	}
	result := model.EventPage{Meta: responseMeta(query), TotalCount: int64(count)}
	hasMore := len(events) > query.PageSize
	if hasMore {
		events = events[:query.PageSize]
	}
	result.Events = events
	if hasMore {
		last := events[len(events)-1]
		result.Meta.NextCursor, err = s.config.CursorCodec.Encode(model.Cursor{Version: 1, Operation: model.OperationEvents, Selection: selection, RequestedAt: &last.RequestedAt, AttemptID: last.AttemptID})
		if err != nil {
			return model.EventPage{}, err
		}
	}
	s.s2LegacyEnrichArrivalEstimates(ctx, result.Events)
	return result, nil
}

func (s *Store) s2LegacyEnrichArrivalEstimates(ctx context.Context, events []model.Event) {
	s.s2LegacyRefreshOutputTiming(ctx, events)
	for i := range events {
		event := &events[i]
		event.EnrichThroughput()
		event.ThroughputEstimate = nil
		estimate := model.EstimateArrivalThroughput(*event, nil)
		if estimate.Reason == "observed_available" {
			continue
		}
		if estimate.Reason == "insufficient_history" {
			history, err := s.s2LegacyArrivalEstimateHistory(ctx, *event)
			if err != nil {
				estimate.Reason = "history_unavailable"
			} else {
				estimate = model.EstimateArrivalThroughput(*event, history)
			}
		}
		event.ThroughputEstimate = &estimate
	}
}

func (s *Store) s2LegacyRefreshOutputTiming(ctx context.Context, events []model.Event) {
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
	selection, err := s.s2LegacyRawSelection(ctx, q, "attempt_id", "output_timing")
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

func (s *Store) s2LegacyTimingMetrics(ctx context.Context, q model.Query, percentiles bool) (map[string]model.TimingMetric, error) {
	columns := []string{"latency_ms", "first_token_latency_ms", "provider_latency_ms", "time_to_first_token_ms", "generation_time_ms"}
	names := []string{"e2e", "latency", "provider_latency", "ttft", "generation"}
	sources := []string{"observed", "observed_dispatch_to_first_token", "observed_dispatch_to_response", "observed", "observed_first_to_last_arrival"}
	selection, err := s.s2LegacyRawSelection(ctx, q, append(columns, "outcome")...)
	if err != nil {
		return nil, err
	}
	selects := make([]string, 0, 25)
	type metricValues struct {
		count          int64
		total, maximum *int64
		median, p95    *float64
	}
	values := make([]metricValues, len(columns))
	args := []any{}
	for i, c := range columns {
		selects = append(selects, "toInt64(count("+c+"))", "sumOrNull("+c+")", "maxOrNull("+c+")")
		args = append(args, &values[i].count, &values[i].total, &values[i].maximum)
		if percentiles {
			// The median averages the central rows. P95 uses SQLite's
			// ceil(.95*n)-1 index, which differs from ClickHouse's default quantile.
			sorted := "arraySort(groupArray(" + c + "))"
			selects = append(selects, "if(count("+c+")=0,NULL,arrayReduce('avg',arraySlice("+sorted+",intDiv(count("+c+")-1,2)+1,2-count("+c+")%2)))", "if(count("+c+")=0,NULL,toFloat64(arrayElement("+sorted+",intDiv(95*count("+c+")+99,100))))")
			args = append(args, &values[i].median, &values[i].p95)
		}
	}
	err = s.db.Conn().QueryRow(ctx, "SELECT "+strings.Join(selects, ",")+" FROM ("+selection.SQL+") WHERE outcome IS NULL OR outcome<>'canceled'", selection.Args...).Scan(args...)
	if err != nil {
		return nil, err
	}
	result := make(map[string]model.TimingMetric, len(columns))
	for i, name := range names {
		v := values[i]
		result[name] = model.TimingMetric{Source: sources[i], SampleCount: v.count, TotalMS: v.total, MaxMS: v.maximum, MedianMS: v.median, P95MS: v.p95}
	}
	return result, nil
}

func (s *Store) s2LegacyValidateYearActivity(ctx context.Context, q model.Query) error {
	all := q
	all.Start = time.Unix(0, -9223372036854775808).UTC()
	all.End = time.Unix(0, 9223372036854775807).UTC()
	all.Filters = nil
	retained, err := s.s2LegacyRetainedSelection(ctx, all, false, "bucket_start_ns")
	if err != nil {
		return err
	}
	zone, ok, err := s.metadata(ctx, "retention_time_zone")
	if err != nil {
		return err
	}
	if !ok || zone == "" {
		zone = "UTC"
	}
	dq := q
	dq.TimeZone = zone
	dq.BucketWidth = "1d"
	day := coreBucketExpression("bucket_start_ns", dq)
	dayStart, _, err := aggregate.BucketBounds(q.Start, zone, "1d")
	if err != nil {
		return err
	}
	rows, err := s.db.Conn().Query(ctx, "SELECT DISTINCT day FROM (SELECT "+day+" AS day FROM ("+retained.SQL+")) WHERE day>=? AND day<?", append(retained.Args, dayStart.UnixNano(), q.End.UnixNano())...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	b, err := aggregate.NewBucketer(zone, "1d")
	if err != nil {
		return err
	}
	for rows.Next() {
		var ns int64
		if err := rows.Scan(&ns); err != nil {
			return err
		}
		start, end, err := b.Bounds(time.Unix(0, ns).UTC())
		if err != nil {
			return err
		}
		if !start.Before(q.End) || !end.After(q.Start) {
			continue
		}
		if zone != q.TimeZone {
			return RetainedTimeZoneError{StorageTimeZone: zone, QueryTimeZone: q.TimeZone, BucketWidth: "1d"}
		}
		if len(q.Filters) > 0 {
			return fmt.Errorf("activity year filters are unavailable for retained daily stats")
		}
		if start.Before(q.Start) || end.After(q.End) {
			return ErrRetainedRangePartial
		}
	}
	return rows.Err()
}
