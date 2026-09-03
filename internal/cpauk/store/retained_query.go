package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

const rollupTotalsSelect = `0, COALESCE(SUM(upstream_attempts),0),
COALESCE(SUM(CASE WHEN succeeded THEN upstream_attempts ELSE 0 END),0),
COALESCE(SUM(CASE WHEN succeeded THEN 0 ELSE upstream_attempts END),0),
COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(cached_tokens),0),
COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0),
COALESCE(SUM(total_tokens),0), COALESCE(SUM(known_cost_nano),0),
COALESCE(SUM(unpriced_tokens),0),
CASE WHEN SUM(CASE WHEN token_quality='missing' THEN upstream_attempts ELSE 0 END)>0 THEN 'missing'
WHEN SUM(CASE WHEN token_quality='estimated' THEN upstream_attempts ELSE 0 END)>0 THEN 'estimated'
ELSE 'exact' END`

func (s *SQLiteStore) dimensionTotals(ctx context.Context, query model.Query) (map[string]totals, error) {
	return s.dimensionTotalsFor(ctx, query, query.Dimension)
}

func (s *SQLiteStore) dimensionTotalsFor(ctx context.Context, query model.Query, dimension string) (map[string]totals, error) {
	rawExpression, ok := dimensionExpression(dimension)
	if !ok {
		return nil, fmt.Errorf("unsupported dimension %q", dimension)
	}
	rollupExpression, ok := rollupDimensionExpression(dimension)
	if !ok {
		return nil, fmt.Errorf("unsupported retained dimension %q", dimension)
	}
	result := map[string]totals{}
	rawWhere, rawArguments, err := buildWhere(query)
	if err != nil {
		return nil, err
	}
	if err := scanGroupedTotals(ctx, s.db, `SELECT `+rawExpression+`, `+totalsSelect+` FROM events `+rawWhere+` GROUP BY `+rawExpression, rawArguments, result); err != nil {
		return nil, fmt.Errorf("query analytics dimensions: %w", err)
	}
	rollupWhere, rollupArguments, err := buildRollupWhere(query, "bucket_start_ns", "bucket_end_ns")
	if err != nil {
		return nil, err
	}
	if err := scanGroupedTotals(ctx, s.db, `SELECT `+rollupExpression+`, `+rollupTotalsSelect+` FROM rollups `+rollupWhere+` GROUP BY `+rollupExpression, rollupArguments, result); err != nil {
		return nil, fmt.Errorf("query retained analytics dimensions: %w", err)
	}
	return result, nil
}

func scanGroupedTotals(ctx context.Context, database *sql.DB, statement string, arguments []any, result map[string]totals) error {
	rows, err := database.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var value, quality string
		var data totals
		if err := rows.Scan(&value, &data.proxyRequests, &data.attempts, &data.succeeded, &data.failed,
			&data.tokens.Input, &data.tokens.Output, &data.tokens.Reasoning, &data.tokens.Cached,
			&data.tokens.CacheRead, &data.tokens.CacheCreation, &data.tokens.Total,
			&data.knownCost, &data.unpriced, &quality); err != nil {
			return err
		}
		data.tokens.Schema, data.tokens.Quality = "normalized-v1", model.TokenQuality(quality)
		current := result[value]
		addTotals(&current, data)
		current.tokens.Schema = "normalized-v1"
		result[value] = current
	}
	return rows.Err()
}

func (s *SQLiteStore) dimensionRequestCounts(ctx context.Context, query model.Query) (map[string]int64, error) {
	return s.dimensionRequestCountsFor(ctx, query, query.Dimension)
}

func (s *SQLiteStore) dimensionRequestCountsFor(ctx context.Context, query model.Query, dimension string) (map[string]int64, error) {
	rawExpression, ok := dimensionExpression(dimension)
	if !ok {
		return nil, fmt.Errorf("unsupported dimension %q", dimension)
	}
	rollupExpression, ok := rollupDimensionExpression(dimension)
	if !ok {
		return nil, fmt.Errorf("unsupported retained dimension %q", dimension)
	}
	sets := map[string]map[string]struct{}{}
	rawWhere, rawArguments, err := buildWhere(query)
	if err != nil {
		return nil, err
	}
	if err := scanDimensionRequests(ctx, s.db, `SELECT `+rawExpression+`, proxy_request_id FROM events `+rawWhere+` GROUP BY `+rawExpression+`, proxy_request_id`, rawArguments, sets); err != nil {
		return nil, fmt.Errorf("query analytics dimension requests: %w", err)
	}
	rollupWhere, rollupArguments, err := buildRollupWhere(query, "bucket_start_ns", "bucket_end_ns")
	if err != nil {
		return nil, err
	}
	if err := scanDimensionRequests(ctx, s.db, `SELECT `+rollupExpression+`, proxy_request_id FROM request_rollups `+rollupWhere+` GROUP BY `+rollupExpression+`, proxy_request_id`, rollupArguments, sets); err != nil {
		return nil, fmt.Errorf("query retained analytics dimension requests: %w", err)
	}
	result := make(map[string]int64, len(sets))
	for value, requests := range sets {
		result[value] = int64(len(requests))
	}
	return result, nil
}

func scanDimensionRequests(ctx context.Context, database *sql.DB, statement string, arguments []any, result map[string]map[string]struct{}) error {
	rows, err := database.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var value, requestID string
		if err := rows.Scan(&value, &requestID); err != nil {
			return err
		}
		if result[value] == nil {
			result[value] = map[string]struct{}{}
		}
		result[value][requestID] = struct{}{}
	}
	return rows.Err()
}

func rollupDimensionExpression(dimension string) (string, bool) {
	expressions := map[string]string{
		"provider": "provider", "model": "model", "credential": "CASE WHEN credential_id='' THEN 'Unknown' ELSE credential_id END",
		"key": "key_id", "endpoint": "endpoint_class", "failure": "CASE WHEN error_class='' THEN 'success' ELSE error_class END",
		"latency": "latency_bucket", "cache": "cache_class",
		"service_tier": "CASE WHEN service_tier='' THEN 'Unknown' ELSE service_tier END",
		"source":       "CASE WHEN import_batch_id='' THEN 'native' ELSE 'import' END",
	}
	expression, ok := expressions[dimension]
	return expression, ok
}

func (s *SQLiteStore) addRetainedTimeseries(ctx context.Context, query model.Query, points map[int64]*pointState) error {
	where, arguments, err := buildRollupWhere(query, "bucket_start_ns", "bucket_end_ns")
	if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT grain,bucket_start_ns,upstream_attempts,
input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,
cache_creation_tokens,total_tokens,token_quality,known_cost_nano,unpriced_tokens
FROM rollups `+where, arguments...)
	if err != nil {
		return fmt.Errorf("query retained analytics timeseries: %w", err)
	}
	for rows.Next() {
		var grain, quality string
		var bucketStart, attempts, input, output, reasoning, cached, cacheRead, cacheCreation, totalTokens, knownCost, unpriced int64
		if err := rows.Scan(&grain, &bucketStart, &attempts, &input, &output, &reasoning, &cached,
			&cacheRead, &cacheCreation, &totalTokens, &quality, &knownCost, &unpriced); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan retained analytics timeseries: %w", err)
		}
		if err := validateRollupWidth(grain, query.BucketWidth); err != nil {
			_ = rows.Close()
			return err
		}
		start, end, err := aggregate.BucketBounds(time.Unix(0, bucketStart).UTC(), query.TimeZone, query.BucketWidth)
		if err != nil {
			_ = rows.Close()
			return err
		}
		state := ensurePoint(points, start, end)
		state.point.UpstreamAttempts += attempts
		state.point.Tokens.Input += input
		state.point.Tokens.Output += output
		state.point.Tokens.Reasoning += reasoning
		state.point.Tokens.Cached += cached
		state.point.Tokens.CacheRead += cacheRead
		state.point.Tokens.CacheCreation += cacheCreation
		state.point.Tokens.Total += totalTokens
		state.point.KnownCost += model.NanoUSD(knownCost)
		state.point.UnpricedTokens += unpriced
		state.quality = combineQuality(state.quality, model.TokenQuality(quality))
	}
	if err := rows.Close(); err != nil {
		return err
	}
	requestRows, err := s.db.QueryContext(ctx, `SELECT grain,bucket_start_ns,proxy_request_id FROM request_rollups `+where, arguments...)
	if err != nil {
		return fmt.Errorf("query retained analytics timeseries requests: %w", err)
	}
	defer func() { _ = requestRows.Close() }()
	for requestRows.Next() {
		var grain, requestID string
		var bucketStart int64
		if err := requestRows.Scan(&grain, &bucketStart, &requestID); err != nil {
			return err
		}
		if err := validateRollupWidth(grain, query.BucketWidth); err != nil {
			return err
		}
		start, end, err := aggregate.BucketBounds(time.Unix(0, bucketStart).UTC(), query.TimeZone, query.BucketWidth)
		if err != nil {
			return err
		}
		ensurePoint(points, start, end).requests[requestID] = struct{}{}
	}
	return requestRows.Err()
}

func ensurePoint(points map[int64]*pointState, start, end time.Time) *pointState {
	state := points[start.UnixNano()]
	if state == nil {
		state = &pointState{point: model.TimeseriesPoint{Start: start, End: end}, requests: map[string]struct{}{}, quality: model.TokenQualityExact}
		points[start.UnixNano()] = state
	}
	return state
}

func validateRollupWidth(grain, width string) error {
	minimum := time.Hour
	if grain == "daily" {
		minimum = 24 * time.Hour
	}
	if width == "1d" || width == "1w" {
		return nil
	}
	duration, err := time.ParseDuration(width)
	if err != nil || duration < minimum {
		return fmt.Errorf("%s retained analytics cannot answer %s buckets exactly", grain, width)
	}
	return nil
}

func (s *SQLiteStore) validateRetainedRange(ctx context.Context, query model.Query) error {
	location, err := s.retentionLocation(ctx)
	if err != nil {
		return err
	}
	predicate, predicateArguments, err := retainedSelectionPredicate(query)
	if err != nil {
		return err
	}
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rollups
WHERE bucket_start_ns < ? AND bucket_end_ns > ?
AND NOT (bucket_start_ns >= ? AND bucket_end_ns <= ?)`+predicate, append([]any{query.End.UnixNano(), query.Start.UnixNano(),
		query.Start.UnixNano(), query.End.UnixNano()}, predicateArguments...)...).Scan(&count); err != nil {
		return fmt.Errorf("validate retained analytics range: %w", err)
	}
	if count != 0 {
		return ErrRetainedRangePartial
	}
	if location.String() != query.TimeZone {
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rollups
WHERE bucket_start_ns < ? AND bucket_end_ns > ?`+predicate,
			append([]any{query.End.UnixNano(), query.Start.UnixNano()}, predicateArguments...)...).Scan(&count); err != nil {
			return fmt.Errorf("validate retained analytics time zone: %w", err)
		}
		if count != 0 {
			return RetainedTimeZoneError{StorageTimeZone: location.String(), QueryTimeZone: query.TimeZone, BucketWidth: query.BucketWidth}
		}
	}
	return nil
}

func retainedSelectionPredicate(query model.Query) (string, []any, error) {
	where, arguments, err := buildRollupWhere(query, "bucket_start_ns", "bucket_end_ns")
	if err != nil {
		return "", nil, err
	}
	const rangePredicate = "WHERE bucket_start_ns >= ? AND bucket_end_ns <= ?"
	if !strings.HasPrefix(where, rangePredicate) || len(arguments) < 2 {
		return "", nil, fmt.Errorf("build retained analytics selection predicate")
	}
	return strings.TrimPrefix(where, rangePredicate), arguments[2:], nil
}
