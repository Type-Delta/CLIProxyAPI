package chstore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// readSelection is a parameterized SELECT. Callers hold s.mu and may wrap SQL
// in a subquery, add aggregation/order/limit, and append parameters in SQL order.
// Columns and expressions passed by readers are trusted source code, never user input.
type readSelection struct {
	SQL  string
	Args []any
}

const eventOutcomeSQL = `COALESCE(outcome, CASE WHEN succeeded = 1 THEN 'succeeded'
WHEN trimBoth(upstream_error_body) = 'context canceled'
OR (startsWith(trimBoth(upstream_error_body), 'Post "') AND endsWith(trimBoth(upstream_error_body), '": context canceled'))
OR (startsWith(trimBoth(upstream_error_body), 'Get "') AND endsWith(trimBoth(upstream_error_body), '": context canceled')) THEN 'canceled' ELSE 'failed' END)`

// rawSelection returns effective event columns, with scalar filtering in SQL and
// first-wins response patches applied before the outer filter. It never selects
// event_json unless requested. Query cursor bounds belong to the page query only.
func (s *Store) rawSelection(ctx context.Context, q model.Query, columns ...string) (readSelection, error) {
	bounds, err := s.readCutoffs(ctx)
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

type readCutoffs struct {
	raw, hourly       int64
	rawSet, hourlySet bool
}

func (s *Store) readCutoffs(ctx context.Context) (readCutoffs, error) {
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
func (s *Store) retainedSelection(ctx context.Context, q model.Query, requests bool, columns ...string) (readSelection, error) {
	where, args, err := buildRollupWhere(q, "bucket_start_ns", "bucket_end_ns")
	if err != nil {
		return readSelection{}, err
	}
	bounds, err := s.readCutoffs(ctx)
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
func (s *Store) validateRetainedRange(ctx context.Context, q model.Query) error {
	selectionQ := q
	selectionQ.Start = time.Unix(0, -9223372036854775808).UTC()
	selectionQ.End = time.Unix(0, 9223372036854775807).UTC()
	selected, err := s.retainedSelection(ctx, selectionQ, false, "bucket_start_ns", "bucket_end_ns", "grain")
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
		rows, err := s.db.Conn().Query(ctx, "SELECT DISTINCT grain FROM ("+selected.SQL+") WHERE bucket_start_ns<? AND bucket_end_ns>?", append(selected.Args, q.End.UnixNano(), q.Start.UnixNano())...)
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

func buildWhere(query model.Query) (string, []any, error) {
	clauses := []string{"requested_at_ns >= ?", "requested_at_ns < ?"}
	arguments := []any{query.Start.UnixNano(), query.End.UnixNano()}
	if len(query.KeyIDs) != 0 {
		clauses = append(clauses, inClause("key_id", len(query.KeyIDs)))
		for _, keyID := range query.KeyIDs {
			arguments = append(arguments, keyID)
		}
	}
	columns := map[string]string{
		"provider": "provider", "model": "model", "credential_id": "credential_id",
		"endpoint_class": "endpoint_class", "auth_type": "auth_type",
		"service_tier": "COALESCE(service_tier_used, service_tier_requested)",
		"error_class":  "error_class", "status_code": "upstream_status_code",
		"token_quality": "token_quality", "session_id": "session_id",
	}
	for _, name := range sortedFilterNames(query.Filters) {
		raw := query.Filters[name]
		if name == "success" || name == "generated" {
			var value bool
			if err := json.Unmarshal(raw, &value); err != nil {
				return "", nil, fmt.Errorf("decode %s filter: %w", name, err)
			}
			column := name
			if name == "success" {
				column = "succeeded"
				if query.Operation == model.OperationEvents {
					column = "(" + eventOutcomeSQL + " <> 'failed')"
				}
			}
			clauses = append(clauses, column+" = ?")
			arguments = append(arguments, value)
			continue
		}
		if name == "outcome" {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return "", nil, err
			}
			clauses = append(clauses, eventOutcomeSQL+" = ?")
			arguments = append(arguments, value)
			continue
		}
		if name == "result" {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return "", nil, fmt.Errorf("decode result filter: %w", err)
			}
			column := "succeeded"
			if query.Operation == model.OperationEvents {
				column = "(" + eventOutcomeSQL + " <> 'failed')"
			}
			clauses = append(clauses, column+" = ?")
			arguments = append(arguments, value == "success")
			continue
		}
		if name == "source" {
			var values []string
			if err := json.Unmarshal(raw, &values); err != nil {
				var value string
				if errScalar := json.Unmarshal(raw, &value); errScalar != nil {
					return "", nil, fmt.Errorf("decode source filter: %w", err)
				}
				values = []string{value}
			}
			var sourceClauses []string
			var batchIDs []string
			for _, value := range values {
				switch value {
				case "native":
					sourceClauses = append(sourceClauses, "import_batch_id IS NULL")
				case "import":
					sourceClauses = append(sourceClauses, "import_batch_id IS NOT NULL")
				default:
					batchIDs = append(batchIDs, value)
				}
			}
			if len(batchIDs) != 0 {
				sourceClauses = append(sourceClauses, inClause("import_batch_id", len(batchIDs)))
				for _, batchID := range batchIDs {
					arguments = append(arguments, batchID)
				}
			}
			clauses = append(clauses, "("+strings.Join(sourceClauses, " OR ")+")")
			continue
		}
		column, ok := columns[name]
		if !ok {
			return "", nil, fmt.Errorf("unsupported store filter %q", name)
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
				if name != "session_id" || !model.IsDerivedSessionIdentity(value) {
					values = append(values, value)
				}
				if name == "session_id" {
					// Equalities on the indexed column avoid a substring scan. A raw ID
					// deliberately finds matching conversations across every known source.
					for _, source := range model.SessionSources {
						values = append(values, source+":"+value)
					}
				}
			}
		}
		clauses = append(clauses, inClause(column, len(values)))
		arguments = append(arguments, values...)
	}
	return "WHERE " + strings.Join(clauses, " AND "), arguments, nil
}

func buildRollupWhere(query model.Query, startColumn, endColumn string) (string, []any, error) {
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

func sortedFilterNames(filters map[string]json.RawMessage) []string {
	names := make([]string, 0, len(filters))
	for name := range filters {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func inClause(column string, count int) string {
	return column + " IN (" + strings.TrimSuffix(strings.Repeat("?,", count), ",") + ")"
}

func dimensionExpression(dimension string) (string, bool) {
	expressions := map[string]string{
		"provider": "provider", "model": "model", "credential": "COALESCE(credential_id, 'Unknown')",
		"key": "key_id", "endpoint": "endpoint_class", "failure": "COALESCE(error_class, 'success')",
		"latency":      "CASE WHEN latency_ms < 100 THEN '<100ms' WHEN latency_ms < 500 THEN '100-499ms' WHEN latency_ms < 1000 THEN '500-999ms' ELSE '1000ms+' END",
		"cache":        "CASE WHEN cache_read_tokens > 0 THEN 'cached' ELSE 'uncached' END",
		"service_tier": "COALESCE(service_tier_used, service_tier_requested, 'Unknown')",
		"source":       "CASE WHEN import_batch_id IS NULL THEN 'native' ELSE 'import' END",
	}
	expression, ok := expressions[dimension]
	return expression, ok
}
