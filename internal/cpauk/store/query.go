package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

const totalsSelect = `COUNT(DISTINCT proxy_request_id), COUNT(*),
COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(cached_tokens),0),
COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0),
COALESCE(SUM(total_tokens),0), COALESCE(SUM(known_cost_nano),0),
COALESCE(SUM(unpriced_tokens),0),
CASE WHEN SUM(CASE WHEN token_quality = 'missing' THEN 1 ELSE 0 END) > 0 THEN 'missing'
WHEN SUM(CASE WHEN token_quality = 'estimated' THEN 1 ELSE 0 END) > 0 THEN 'estimated'
ELSE 'exact' END`

type totals struct {
	proxyRequests int64
	attempts      int64
	tokens        model.TokenUsage
	knownCost     model.NanoUSD
	unpriced      int64
}

type pointState struct {
	point    model.TimeseriesPoint
	requests map[string]struct{}
	quality  model.TokenQuality
}

func (s *SQLiteStore) Summary(ctx context.Context, query model.Query) (model.Summary, error) {
	if err := s.validateQuery(&query, model.OperationSummary); err != nil {
		return model.Summary{}, err
	}
	where, arguments, err := buildWhere(query)
	if err != nil {
		return model.Summary{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.Summary{}, ErrClosed
	}
	if err := s.validateRetainedRange(ctx, query); err != nil {
		return model.Summary{}, err
	}
	result, err := scanTotals(s.db.QueryRowContext(ctx, "SELECT "+totalsSelect+" FROM events "+where, arguments...))
	if err != nil {
		return model.Summary{}, fmt.Errorf("query analytics summary: %w", err)
	}
	rollup, err := s.rollupTotals(ctx, query)
	if err != nil {
		return model.Summary{}, err
	}
	addTotals(&result, rollup)
	result.proxyRequests, err = s.combinedProxyRequests(ctx, query)
	if err != nil {
		return model.Summary{}, err
	}
	return model.Summary{
		Meta:             responseMeta(query),
		ProxyRequests:    result.proxyRequests,
		UpstreamAttempts: result.attempts,
		Tokens:           result.tokens,
		KnownCost:        result.knownCost,
		UnpricedTokens:   result.unpriced,
	}, nil
}

func (s *SQLiteStore) rollupTotals(ctx context.Context, query model.Query) (totals, error) {
	where, arguments, err := buildRollupWhere(query, "bucket_start_ns", "bucket_end_ns")
	if err != nil {
		return totals{}, err
	}
	statement := `SELECT 0, COALESCE(SUM(upstream_attempts),0),
COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(cached_tokens),0),
COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0),
COALESCE(SUM(total_tokens),0), COALESCE(SUM(known_cost_nano),0),
COALESCE(SUM(unpriced_tokens),0),
CASE WHEN SUM(CASE WHEN token_quality = 'missing' THEN upstream_attempts ELSE 0 END) > 0 THEN 'missing'
WHEN SUM(CASE WHEN token_quality = 'estimated' THEN upstream_attempts ELSE 0 END) > 0 THEN 'estimated'
ELSE 'exact' END FROM rollups ` + where
	result, err := scanTotals(s.db.QueryRowContext(ctx, statement, arguments...))
	if err != nil {
		return totals{}, fmt.Errorf("query retained analytics summary: %w", err)
	}
	return result, nil
}

func (s *SQLiteStore) combinedProxyRequests(ctx context.Context, query model.Query) (int64, error) {
	rawWhere, rawArguments, err := buildWhere(query)
	if err != nil {
		return 0, err
	}
	rollupWhere, rollupArguments, err := buildRollupWhere(query, "bucket_start_ns", "bucket_end_ns")
	if err != nil {
		return 0, err
	}
	statement := `SELECT COUNT(DISTINCT proxy_request_id) FROM (
SELECT proxy_request_id FROM events ` + rawWhere + `
UNION ALL SELECT proxy_request_id FROM request_rollups ` + rollupWhere + `)`
	arguments := append(rawArguments, rollupArguments...)
	var count int64
	if err := s.db.QueryRowContext(ctx, statement, arguments...).Scan(&count); err != nil {
		return 0, fmt.Errorf("query distinct analytics requests: %w", err)
	}
	return count, nil
}

func addTotals(destination *totals, source totals) {
	destination.attempts += source.attempts
	destination.tokens.Input += source.tokens.Input
	destination.tokens.Output += source.tokens.Output
	destination.tokens.Reasoning += source.tokens.Reasoning
	destination.tokens.Cached += source.tokens.Cached
	destination.tokens.CacheRead += source.tokens.CacheRead
	destination.tokens.CacheCreation += source.tokens.CacheCreation
	destination.tokens.Total += source.tokens.Total
	destination.knownCost += source.knownCost
	destination.unpriced += source.unpriced
	destination.tokens.Quality = combineQuality(destination.tokens.Quality, source.tokens.Quality)
}

func scanTotals(row *sql.Row) (totals, error) {
	var result totals
	var quality string
	err := row.Scan(&result.proxyRequests, &result.attempts,
		&result.tokens.Input, &result.tokens.Output, &result.tokens.Reasoning,
		&result.tokens.Cached, &result.tokens.CacheRead, &result.tokens.CacheCreation,
		&result.tokens.Total, &result.knownCost, &result.unpriced, &quality)
	result.tokens.Schema = "normalized-v1"
	result.tokens.Quality = model.TokenQuality(quality)
	return result, err
}

func (s *SQLiteStore) Timeseries(ctx context.Context, query model.Query) (model.Timeseries, error) {
	if err := s.validateQuery(&query, model.OperationTimeseries); err != nil {
		return model.Timeseries{}, err
	}
	where, arguments, err := buildWhere(query)
	if err != nil {
		return model.Timeseries{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.Timeseries{}, ErrClosed
	}
	if err := s.validateRetainedRange(ctx, query); err != nil {
		return model.Timeseries{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT requested_at_ns, proxy_request_id,
input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens,
cache_creation_tokens, total_tokens, token_quality, known_cost_nano, unpriced_tokens
FROM events `+where+` ORDER BY requested_at_ns`, arguments...)
	if err != nil {
		return model.Timeseries{}, fmt.Errorf("query analytics timeseries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	points := map[int64]*pointState{}
	for rows.Next() {
		var requestedNS int64
		var requestID, quality string
		var input, output, reasoning, cached, cacheRead, cacheCreation, totalTokens, unpriced int64
		var cost sql.NullInt64
		if err := rows.Scan(&requestedNS, &requestID, &input, &output, &reasoning, &cached, &cacheRead, &cacheCreation, &totalTokens, &quality, &cost, &unpriced); err != nil {
			return model.Timeseries{}, fmt.Errorf("scan analytics timeseries row: %w", err)
		}
		start, end, err := aggregate.BucketBounds(time.Unix(0, requestedNS).UTC(), query.TimeZone, query.BucketWidth)
		if err != nil {
			return model.Timeseries{}, err
		}
		state := points[start.UnixNano()]
		if state == nil {
			state = &pointState{point: model.TimeseriesPoint{Start: start, End: end}, requests: map[string]struct{}{}, quality: model.TokenQualityExact}
			points[start.UnixNano()] = state
		}
		state.requests[requestID] = struct{}{}
		state.point.UpstreamAttempts++
		state.point.Tokens.Input += input
		state.point.Tokens.Output += output
		state.point.Tokens.Reasoning += reasoning
		state.point.Tokens.Cached += cached
		state.point.Tokens.CacheRead += cacheRead
		state.point.Tokens.CacheCreation += cacheCreation
		state.point.Tokens.Total += totalTokens
		if cost.Valid {
			state.point.KnownCost += model.NanoUSD(cost.Int64)
		}
		state.point.UnpricedTokens += unpriced
		state.quality = combineQuality(state.quality, model.TokenQuality(quality))
	}
	if err := rows.Err(); err != nil {
		return model.Timeseries{}, fmt.Errorf("read analytics timeseries rows: %w", err)
	}
	if err := s.addRetainedTimeseries(ctx, query, points); err != nil {
		return model.Timeseries{}, err
	}
	keys := make([]int64, 0, len(points))
	for key := range points {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	result := model.Timeseries{Meta: responseMeta(query), Points: make([]model.TimeseriesPoint, 0, len(keys))}
	for _, key := range keys {
		state := points[key]
		state.point.ProxyRequests = int64(len(state.requests))
		state.point.Tokens.Schema = "normalized-v1"
		state.point.Tokens.Quality = state.quality
		result.Points = append(result.Points, state.point)
	}
	return result, nil
}

func (s *SQLiteStore) Dimensions(ctx context.Context, query model.Query) (model.DimensionPage, error) {
	if err := s.validateQuery(&query, model.OperationDimensions); err != nil {
		return model.DimensionPage{}, err
	}
	if _, ok := dimensionExpression(query.Dimension); !ok {
		return model.DimensionPage{}, fmt.Errorf("unsupported dimension %q", query.Dimension)
	}
	selection, err := query.SelectionDigest()
	if err != nil {
		return model.DimensionPage{}, err
	}
	var cursor model.Cursor
	if query.Cursor != "" {
		cursor, err = s.decodeCursor(query.Cursor)
		if err != nil {
			return model.DimensionPage{}, err
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.DimensionPage{}, ErrClosed
	}
	if err := s.validateRetainedRange(ctx, query); err != nil {
		return model.DimensionPage{}, err
	}
	grouped, err := s.dimensionTotals(ctx, query)
	if err != nil {
		return model.DimensionPage{}, err
	}
	requestCounts, err := s.dimensionRequestCounts(ctx, query)
	if err != nil {
		return model.DimensionPage{}, err
	}
	allRows := make([]model.DimensionRow, 0, len(grouped))
	for value, data := range grouped {
		allRows = append(allRows, model.DimensionRow{Value: value, ProxyRequests: requestCounts[value],
			UpstreamAttempts: data.attempts, Tokens: data.tokens, KnownCost: data.knownCost, UnpricedTokens: data.unpriced})
	}
	slices.SortFunc(allRows, func(left, right model.DimensionRow) int {
		if metric := cmp.Compare(right.Tokens.Total, left.Tokens.Total); metric != 0 {
			return metric
		}
		return cmp.Compare(left.Value, right.Value)
	})
	start := 0
	if cursor.Rank > 0 {
		metric, _ := strconv.ParseInt(cursor.Metric, 10, 64)
		for start < len(allRows) && (allRows[start].Tokens.Total > metric || allRows[start].Tokens.Total == metric && allRows[start].Value <= cursor.Value) {
			start++
		}
	}
	end := start + query.PageSize
	hasMore := end < len(allRows)
	if end > len(allRows) {
		end = len(allRows)
	}
	result := model.DimensionPage{Meta: responseMeta(query), Dimension: query.Dimension, Rows: append([]model.DimensionRow(nil), allRows[start:end]...)}
	if hasMore && len(result.Rows) != 0 {
		last := result.Rows[len(result.Rows)-1]
		next := model.Cursor{Version: 1, Operation: model.OperationDimensions, Selection: selection, Metric: strconv.FormatInt(last.Tokens.Total, 10), Value: last.Value, Rank: cursor.Rank + len(result.Rows)}
		result.Meta.NextCursor, err = s.encodeCursor(next)
		if err != nil {
			return model.DimensionPage{}, err
		}
	}
	return result, nil
}

const eventSelect = `schema_version, attempt_id, proxy_request_id, request_id_quality, key_id,
requested_at_ns, provider, executor_type, model, requested_alias, endpoint_class, auth_type,
credential_id, credential_id_algorithm, succeeded, upstream_status_code, error_class,
latency_ms, time_to_first_token_ms, service_tier_requested, service_tier_used, generated,
input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens,
cache_creation_tokens, total_tokens, accounting_schema, token_quality, known_cost_nano, unpriced_tokens`

func (s *SQLiteStore) Events(ctx context.Context, query model.Query) (model.EventPage, error) {
	if err := s.validateQuery(&query, model.OperationEvents); err != nil {
		return model.EventPage{}, err
	}
	where, arguments, err := buildWhere(query)
	if err != nil {
		return model.EventPage{}, err
	}
	selection, err := query.SelectionDigest()
	if err != nil {
		return model.EventPage{}, err
	}
	var cursor model.Cursor
	if query.Cursor != "" {
		cursor, err = s.decodeCursor(query.Cursor)
		if err != nil {
			return model.EventPage{}, err
		}
		where += " AND (requested_at_ns < ? OR (requested_at_ns = ? AND attempt_id > ?))"
		ns := cursor.RequestedAt.UnixNano()
		arguments = append(arguments, ns, ns, cursor.AttemptID)
	}
	arguments = append(arguments, query.PageSize+1)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.EventPage{}, ErrClosed
	}
	if !s.retentionCutoff.IsZero() && query.Start.Before(s.retentionCutoff) {
		return model.EventPage{}, ErrRetainedRangePartial
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+eventSelect+" FROM events "+where+" ORDER BY requested_at_ns DESC, attempt_id ASC LIMIT ?", arguments...)
	if err != nil {
		return model.EventPage{}, fmt.Errorf("query analytics events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := model.EventPage{Meta: responseMeta(query)}
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return model.EventPage{}, err
		}
		result.Events = append(result.Events, event)
	}
	if err := rows.Err(); err != nil {
		return model.EventPage{}, fmt.Errorf("read analytics events: %w", err)
	}
	if len(result.Events) > query.PageSize {
		result.Events = result.Events[:query.PageSize]
		last := result.Events[len(result.Events)-1]
		cursor := model.Cursor{Version: 1, Operation: model.OperationEvents, Selection: selection, RequestedAt: &last.RequestedAt, AttemptID: last.AttemptID}
		result.Meta.NextCursor, err = s.encodeCursor(cursor)
		if err != nil {
			return model.EventPage{}, err
		}
	}
	return result, nil
}

func (s *SQLiteStore) EventByAttemptID(ctx context.Context, attemptID string, query model.Query) (model.Event, bool, error) {
	if !model.IsCorrelationID(attemptID) {
		return model.Event{}, false, fmt.Errorf("invalid analytics attempt ID")
	}
	query.Cursor = ""
	if err := s.validateQuery(&query, model.OperationEvents); err != nil {
		return model.Event{}, false, err
	}
	where, arguments, err := buildWhere(query)
	if err != nil {
		return model.Event{}, false, err
	}
	where += " AND attempt_id = ?"
	arguments = append(arguments, attemptID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.Event{}, false, ErrClosed
	}
	if !s.retentionCutoff.IsZero() && query.Start.Before(s.retentionCutoff) {
		return model.Event{}, false, ErrRetainedRangePartial
	}
	event, err := scanEvent(s.db.QueryRowContext(ctx, "SELECT "+eventSelect+" FROM events "+where+" LIMIT 1", arguments...))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Event{}, false, nil
	}
	if err != nil {
		return model.Event{}, false, err
	}
	return event, true, nil
}

func (s *SQLiteStore) Leaderboard(ctx context.Context, query model.Query) (model.LeaderboardPage, error) {
	if err := s.validateQuery(&query, model.OperationLeaderboard); err != nil {
		return model.LeaderboardPage{}, err
	}
	selection, err := query.SelectionDigest()
	if err != nil {
		return model.LeaderboardPage{}, err
	}
	var cursor model.Cursor
	if query.Cursor != "" {
		cursor, err = s.decodeCursor(query.Cursor)
		if err != nil {
			return model.LeaderboardPage{}, err
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return model.LeaderboardPage{}, ErrClosed
	}
	if err := s.validateRetainedRange(ctx, query); err != nil {
		return model.LeaderboardPage{}, err
	}
	grouped, err := s.dimensionTotalsFor(ctx, query, "key")
	if err != nil {
		return model.LeaderboardPage{}, err
	}
	requestCounts, err := s.dimensionRequestCountsFor(ctx, query, "key")
	if err != nil {
		return model.LeaderboardPage{}, err
	}
	allRows := make([]model.LeaderboardRow, 0, len(grouped))
	for keyID, data := range grouped {
		allRows = append(allRows, model.LeaderboardRow{KeyID: keyID, ProxyRequests: requestCounts[keyID],
			UpstreamAttempts: data.attempts, Tokens: data.tokens, KnownCost: data.knownCost, UnpricedTokens: data.unpriced})
	}
	model.SortLeaderboard(allRows, query.SortBy)
	fullIDs := make([]string, len(allRows))
	for index := range allRows {
		fullIDs[index] = allRows[index].KeyID
	}
	shortIDs, err := model.ShortKeyIDs(fullIDs)
	if err != nil {
		return model.LeaderboardPage{}, err
	}
	var totalMetric int64
	for index := range allRows {
		allRows[index].ShortKeyID = shortIDs[allRows[index].KeyID]
		if query.SortBy == model.LeaderboardSortCost {
			totalMetric += int64(allRows[index].KnownCost)
		} else {
			totalMetric += allRows[index].Tokens.Total
		}
	}
	for index := range allRows {
		metric := allRows[index].Tokens.Total
		if query.SortBy == model.LeaderboardSortCost {
			metric = int64(allRows[index].KnownCost)
		}
		allRows[index].PercentOfTotal = percentage(metric, totalMetric)
	}
	start := 0
	if cursor.Rank > 0 {
		cursorMetric, err := leaderboardCursorMetric(cursor)
		if err != nil {
			return model.LeaderboardPage{}, err
		}
		found := false
		for index := range allRows {
			metric := any(allRows[index].Tokens.Total)
			if query.SortBy == model.LeaderboardSortCost {
				metric = int64(allRows[index].KnownCost)
			}
			if allRows[index].KeyID == cursor.KeyID && metric == cursorMetric {
				start = index + 1
				found = true
				break
			}
		}
		if !found {
			return model.LeaderboardPage{}, fmt.Errorf("leaderboard cursor is stale")
		}
	}
	end := start + query.PageSize
	hasMore := end < len(allRows)
	if end > len(allRows) {
		end = len(allRows)
	}
	result := model.LeaderboardPage{Meta: responseMeta(query), SortBy: query.SortBy, Rows: append([]model.LeaderboardRow(nil), allRows[start:end]...)}
	if hasMore && len(result.Rows) != 0 {
		last := result.Rows[len(result.Rows)-1]
		metric := strconv.FormatInt(last.Tokens.Total, 10)
		if query.SortBy == model.LeaderboardSortCost {
			metric = last.KnownCost.String()
		}
		cursor := model.Cursor{Version: 1, Operation: model.OperationLeaderboard, SortBy: query.SortBy, Selection: selection, Metric: metric, KeyID: last.KeyID, Rank: last.Rank}
		result.Meta.NextCursor, err = s.encodeCursor(cursor)
		if err != nil {
			return model.LeaderboardPage{}, err
		}
	}
	return result, nil
}

func (s *SQLiteStore) leaderboardShortIDs(ctx context.Context, query model.Query) (map[string]string, error) {
	withoutCursor := query
	withoutCursor.Cursor = ""
	where, arguments, err := buildWhere(withoutCursor)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT DISTINCT key_id FROM events "+where, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query leaderboard key identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var fullIDs []string
	for rows.Next() {
		var keyID string
		if err := rows.Scan(&keyID); err != nil {
			return nil, fmt.Errorf("scan leaderboard key identity: %w", err)
		}
		fullIDs = append(fullIDs, keyID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read leaderboard key identities: %w", err)
	}
	return model.ShortKeyIDs(fullIDs)
}

func (s *SQLiteStore) leaderboardTotal(ctx context.Context, query model.Query, sort model.LeaderboardSort) (int64, error) {
	withoutCursor := query
	withoutCursor.Cursor = ""
	where, arguments, err := buildWhere(withoutCursor)
	if err != nil {
		return 0, err
	}
	expression := "COALESCE(SUM(total_tokens),0)"
	if sort == model.LeaderboardSortCost {
		expression = "COALESCE(SUM(known_cost_nano),0)"
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT "+expression+" FROM events "+where, arguments...).Scan(&total); err != nil {
		return 0, fmt.Errorf("query analytics leaderboard total: %w", err)
	}
	return total, nil
}

func leaderboardCursorMetric(cursor model.Cursor) (any, error) {
	if cursor.SortBy == model.LeaderboardSortCost {
		value, err := model.ParseNanoUSD(cursor.Metric)
		return int64(value), err
	}
	return strconv.ParseInt(cursor.Metric, 10, 64)
}

func percentage(value, total int64) string {
	if total <= 0 || value <= 0 {
		return "0"
	}
	numerator := new(big.Int).Mul(big.NewInt(value), big.NewInt(100))
	ratio := new(big.Rat).SetFrac(numerator, big.NewInt(total))
	text := ratio.FloatString(9)
	return strings.TrimRight(strings.TrimRight(text, "0"), ".")
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
		"token_quality": "token_quality",
	}
	for name, raw := range query.Filters {
		if name == "success" || name == "generated" {
			var value bool
			if err := json.Unmarshal(raw, &value); err != nil {
				return "", nil, fmt.Errorf("decode %s filter: %w", name, err)
			}
			clauses = append(clauses, name+" = ?")
			arguments = append(arguments, value)
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
				values = append(values, value)
			}
		}
		clauses = append(clauses, inClause(column, len(values)))
		arguments = append(arguments, values...)
	}
	return "WHERE " + strings.Join(clauses, " AND "), arguments, nil
}

func buildRollupWhere(query model.Query, startColumn, endColumn string) (string, []any, error) {
	clauses := []string{startColumn + " >= ?", endColumn + " <= ?"}
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
	for name, raw := range query.Filters {
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

func inClause(column string, count int) string {
	return column + " IN (" + strings.TrimSuffix(strings.Repeat("?,", count), ",") + ")"
}

func dimensionExpression(dimension string) (string, bool) {
	expressions := map[string]string{
		"provider": "provider", "model": "model", "credential": "COALESCE(credential_id, 'Unknown')",
		"key": "key_id", "endpoint": "endpoint_class", "failure": "COALESCE(error_class, 'success')",
		"latency":      "CASE WHEN latency_ms < 100 THEN '<100ms' WHEN latency_ms < 500 THEN '100-499ms' WHEN latency_ms < 1000 THEN '500-999ms' ELSE '1000ms+' END",
		"cache":        "CASE WHEN cached_tokens > 0 THEN 'cached' ELSE 'uncached' END",
		"service_tier": "COALESCE(service_tier_used, service_tier_requested, 'Unknown')",
	}
	expression, ok := expressions[dimension]
	return expression, ok
}

type eventScanner interface {
	Scan(...any) error
}

func scanEvent(rows eventScanner) (model.Event, error) {
	var event model.Event
	var requestedNS int64
	var requestQuality, tokenQuality string
	var requestedAlias, authType, credentialID, credentialAlgorithm sql.NullString
	var status sql.NullInt64
	var errorClass, tierRequested, tierUsed sql.NullString
	var ttft sql.NullInt64
	var knownCost sql.NullInt64
	err := rows.Scan(&event.SchemaVersion, &event.AttemptID, &event.ProxyRequestID, &requestQuality,
		&event.KeyID, &requestedNS, &event.Provider, &event.ExecutorType, &event.Model, &requestedAlias,
		&event.EndpointClass, &authType, &credentialID, &credentialAlgorithm, &event.Succeeded,
		&status, &errorClass, &event.LatencyMS, &ttft, &tierRequested, &tierUsed, &event.Generated,
		&event.Tokens.Input, &event.Tokens.Output, &event.Tokens.Reasoning, &event.Tokens.Cached,
		&event.Tokens.CacheRead, &event.Tokens.CacheCreation, &event.Tokens.Total,
		&event.Tokens.Schema, &tokenQuality, &knownCost, &event.UnpricedTokens)
	if err != nil {
		return model.Event{}, fmt.Errorf("scan analytics event: %w", err)
	}
	event.RequestIDQuality = model.RequestIDQuality(requestQuality)
	event.RequestedAt = time.Unix(0, requestedNS).UTC()
	event.RequestedAlias = scanNullableString(requestedAlias)
	event.AuthType = scanNullableString(authType)
	event.CredentialID = scanNullableString(credentialID)
	event.CredentialIDAlgorithm = scanNullableString(credentialAlgorithm)
	event.ErrorClass = scanNullableString(errorClass)
	event.ServiceTierRequested = scanNullableString(tierRequested)
	event.ServiceTierUsed = scanNullableString(tierUsed)
	event.Tokens.Quality = model.TokenQuality(tokenQuality)
	if knownCost.Valid {
		value := model.NanoUSD(knownCost.Int64)
		event.KnownCost = &value
	}
	if status.Valid {
		value := int(status.Int64)
		event.UpstreamStatusCode = &value
	}
	if ttft.Valid {
		value := ttft.Int64
		event.TimeToFirstTokenMS = &value
	}
	return event, nil
}

func combineQuality(current, next model.TokenQuality) model.TokenQuality {
	if current == model.TokenQualityMissing || next == model.TokenQualityMissing {
		return model.TokenQualityMissing
	}
	if current == model.TokenQualityEstimated || next == model.TokenQualityEstimated {
		return model.TokenQualityEstimated
	}
	return model.TokenQualityExact
}

func responseMeta(query model.Query) model.ResponseMeta {
	return model.ResponseMeta{SchemaVersion: model.APISchemaVersion, Range: model.Range{Start: query.Start, End: query.End, TimeZone: query.TimeZone}}
}

func (s *SQLiteStore) decodeCursor(value string) (model.Cursor, error) {
	if s.config.CursorCodec == nil {
		return model.Cursor{}, fmt.Errorf("cursor codec is not configured")
	}
	return s.config.CursorCodec.Decode(value)
}

func (s *SQLiteStore) validateQuery(query *model.Query, operation model.Operation) error {
	if query == nil || query.Operation != operation {
		return fmt.Errorf("analytics query operation must be %s", operation)
	}
	if err := query.Validate(); err != nil {
		return err
	}
	if query.Cursor != "" {
		return query.ValidateCursor(s.config.CursorCodec, model.CursorTransportBody)
	}
	return nil
}

func (s *SQLiteStore) encodeCursor(cursor model.Cursor) (string, error) {
	if s.config.CursorCodec == nil {
		return "", fmt.Errorf("cursor codec is not configured")
	}
	return s.config.CursorCodec.Encode(cursor)
}
