package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func (s *SQLiteStore) Activity(ctx context.Context, query model.Query) (model.Activity, error) {
	if err := s.validateQuery(&query, model.OperationActivity); err != nil {
		return model.Activity{}, err
	}
	grain, err := activityGrain(query.Window)
	if err != nil {
		return model.Activity{}, err
	}
	query, err = normalizeYearActivityQuery(query, grain)
	if err != nil {
		return model.Activity{}, err
	}
	buckets, err := s.activityBuckets(ctx, query, grain)
	if err != nil {
		return model.Activity{}, err
	}
	return model.Activity{Meta: responseMeta(query), Grain: grain, Zone: query.TimeZone, Buckets: buckets}, nil
}

func (s *SQLiteStore) activityBuckets(ctx context.Context, query model.Query, width string) ([]model.ActivityBucket, error) {
	if query.Window == "year" && width == "1d" {
		return s.yearActivityBuckets(ctx, query)
	}
	timeseriesQuery := query
	timeseriesQuery.Operation = model.OperationTimeseries
	timeseriesQuery.Window = ""
	timeseriesQuery.BucketWidth = width
	timeseries, err := s.Timeseries(ctx, timeseriesQuery)
	if err != nil {
		return nil, err
	}
	sequence, err := analyticsBucketSequence(query, width)
	if err != nil {
		return nil, err
	}
	buckets := make([]model.ActivityBucket, len(sequence))
	byStart := make(map[int64]*model.ActivityBucket, len(sequence))
	for index, bounds := range sequence {
		buckets[index] = model.ActivityBucket{Start: bounds.start, End: bounds.end}
		byStart[bounds.start.UnixNano()] = &buckets[index]
	}
	for _, point := range timeseries.Points {
		bucket := byStart[point.Start.UnixNano()]
		if bucket == nil {
			continue
		}
		*bucket = model.ActivityBucket{
			Start: point.Start, End: point.End, Requests: point.ProxyRequests,
			InputTokens: point.Tokens.Input, OutputTokens: point.Tokens.Output,
			CachedTokens: point.Tokens.Cached, CacheReadTokens: point.Tokens.CacheRead,
			CacheCreationTokens: point.Tokens.CacheCreation, ReasoningTokens: point.Tokens.Reasoning,
			TotalTokens: point.Tokens.Total, KnownCost: point.KnownCost,
		}
	}
	if err := s.addActivityOutcomes(ctx, query, width, byStart); err != nil {
		return nil, err
	}
	return buckets, nil
}

// A rolling year ends in the current local day but starts at a partial day
// when expressed as 365 elapsed days. Align it to 365 local calendar days
// while keeping the requested end unchanged.
func normalizeYearActivityQuery(query model.Query, width string) (model.Query, error) {
	if query.Window != "year" || width != "1d" || query.End.Sub(query.Start) < 364*24*time.Hour {
		return query, nil
	}
	location, err := time.LoadLocation(query.TimeZone)
	if err != nil {
		return model.Query{}, fmt.Errorf("load activity time zone %q: %w", query.TimeZone, err)
	}
	localEnd := query.End.In(location)
	endDay := time.Date(localEnd.Year(), localEnd.Month(), localEnd.Day(), 0, 0, 0, 0, location)
	lastDay := endDay
	if query.End.Equal(endDay.UTC()) {
		lastDay = endDay.AddDate(0, 0, -1)
	}
	query.Start = lastDay.AddDate(0, 0, -364).UTC()
	return query, nil
}

func (s *SQLiteStore) yearActivityBuckets(ctx context.Context, query model.Query) ([]model.ActivityBucket, error) {
	sequence, err := analyticsBucketSequence(query, "1d")
	if err != nil {
		return nil, err
	}
	buckets := make([]model.ActivityBucket, len(sequence))
	byStart := make(map[int64]*model.ActivityBucket, len(sequence))
	requests := make(map[int64]map[string]struct{}, len(sequence))
	for index, bounds := range sequence {
		buckets[index] = model.ActivityBucket{Start: bounds.start, End: bounds.end}
		byStart[bounds.start.UnixNano()] = &buckets[index]
	}

	rawWhere, rawArguments, err := buildWhere(query)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrClosed
	}
	if err := s.validateDailyStatsRange(ctx, query); err != nil {
		return nil, err
	}
	rawRows, err := s.db.QueryContext(ctx, `SELECT requested_at_ns,proxy_request_id,succeeded,
input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,
cache_creation_tokens,total_tokens,known_cost_nano FROM events `+rawWhere, rawArguments...)
	if err != nil {
		return nil, fmt.Errorf("query year activity events: %w", err)
	}
	for rawRows.Next() {
		var requestedNS int64
		var requestID string
		var succeeded bool
		var input, output, reasoning, cached, cacheRead, cacheCreation, total int64
		var knownCost sql.NullInt64
		if err := rawRows.Scan(&requestedNS, &requestID, &succeeded, &input, &output, &reasoning,
			&cached, &cacheRead, &cacheCreation, &total, &knownCost); err != nil {
			_ = rawRows.Close()
			return nil, fmt.Errorf("scan year activity event: %w", err)
		}
		start, _, errBounds := aggregate.BucketBounds(time.Unix(0, requestedNS).UTC(), query.TimeZone, "1d")
		if errBounds != nil {
			_ = rawRows.Close()
			return nil, errBounds
		}
		bucket := byStart[start.UnixNano()]
		if bucket == nil {
			continue
		}
		if requests[start.UnixNano()] == nil {
			requests[start.UnixNano()] = map[string]struct{}{}
		}
		requests[start.UnixNano()][requestID] = struct{}{}
		addActivityOutcome(bucket, succeeded, 1)
		addActivityTokens(bucket, input, output, reasoning, cached, cacheRead, cacheCreation, total)
		if knownCost.Valid {
			bucket.KnownCost += model.NanoUSD(knownCost.Int64)
		}
	}
	if err := rawRows.Err(); err != nil {
		_ = rawRows.Close()
		return nil, fmt.Errorf("read year activity events: %w", err)
	}
	if err := rawRows.Close(); err != nil {
		return nil, fmt.Errorf("close year activity events: %w", err)
	}
	for start, values := range requests {
		byStart[start].Requests += int64(len(values))
	}

	statsWhere, statsArguments := dailyStatsWhere(query, false)
	statsRows, err := s.db.QueryContext(ctx, `SELECT day_start_ns,requests,succeeded,failed,
input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,
cache_creation_tokens,total_tokens FROM daily_stats `+statsWhere, statsArguments...)
	if err != nil {
		return nil, fmt.Errorf("query retained year activity: %w", err)
	}
	defer func() { _ = statsRows.Close() }()
	for statsRows.Next() {
		var dayStart, requestCount, succeeded, failed int64
		var input, output, reasoning, cached, cacheRead, cacheCreation, total int64
		if err := statsRows.Scan(&dayStart, &requestCount, &succeeded, &failed, &input, &output,
			&reasoning, &cached, &cacheRead, &cacheCreation, &total); err != nil {
			return nil, fmt.Errorf("scan retained year activity: %w", err)
		}
		bucket := byStart[dayStart]
		if bucket == nil {
			continue
		}
		bucket.Requests += requestCount
		bucket.Succeeded += succeeded
		bucket.Failed += failed
		addActivityTokens(bucket, input, output, reasoning, cached, cacheRead, cacheCreation, total)
	}
	if err := statsRows.Err(); err != nil {
		return nil, fmt.Errorf("read retained year activity: %w", err)
	}
	return buckets, nil
}

func (s *SQLiteStore) validateDailyStatsRange(ctx context.Context, query model.Query) error {
	where, arguments := dailyStatsWhere(query, true)
	var overlapping int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM daily_stats "+where, arguments...).Scan(&overlapping); err != nil {
		return fmt.Errorf("validate retained year activity: %w", err)
	}
	if overlapping == 0 {
		return nil
	}
	location, err := s.retentionLocation(ctx)
	if err != nil {
		return err
	}
	if location.String() != query.TimeZone {
		return RetainedTimeZoneError{StorageTimeZone: location.String(), QueryTimeZone: query.TimeZone, BucketWidth: "1d"}
	}
	if len(query.Filters) != 0 {
		return fmt.Errorf("activity year filters are unavailable for retained daily stats")
	}
	partialWhere := where + " AND NOT (day_start_ns >= ? AND day_end_ns <= ?)"
	partialArguments := append(arguments, query.Start.UnixNano(), query.End.UnixNano())
	var partial int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM daily_stats "+partialWhere, partialArguments...).Scan(&partial); err != nil {
		return fmt.Errorf("validate retained year activity range: %w", err)
	}
	if partial != 0 {
		return ErrRetainedRangePartial
	}
	return nil
}

func dailyStatsWhere(query model.Query, overlap bool) (string, []any) {
	comparison := "day_start_ns >= ? AND day_end_ns <= ?"
	if overlap {
		comparison = "day_start_ns < ? AND day_end_ns > ?"
	}
	arguments := []any{query.Start.UnixNano(), query.End.UnixNano()}
	if overlap {
		arguments[0], arguments[1] = query.End.UnixNano(), query.Start.UnixNano()
	}
	if len(query.KeyIDs) == 0 {
		return "WHERE " + comparison, arguments
	}
	for _, keyID := range query.KeyIDs {
		arguments = append(arguments, keyID)
	}
	return "WHERE " + comparison + " AND " + inClause("key_id", len(query.KeyIDs)), arguments
}

func addActivityTokens(bucket *model.ActivityBucket, input, output, reasoning, cached, cacheRead, cacheCreation, total int64) {
	bucket.InputTokens += input
	bucket.OutputTokens += output
	bucket.ReasoningTokens += reasoning
	bucket.CachedTokens += cached
	bucket.CacheReadTokens += cacheRead
	bucket.CacheCreationTokens += cacheCreation
	bucket.TotalTokens += total
}

type analyticsBucket struct {
	start time.Time
	end   time.Time
}

func analyticsBucketSequence(query model.Query, width string) ([]analyticsBucket, error) {
	start, end, err := aggregate.BucketBounds(query.Start, query.TimeZone, width)
	if err != nil {
		return nil, err
	}
	sequence := make([]analyticsBucket, 0)
	for start.Before(query.End) {
		if len(sequence) >= model.MaxBuckets {
			return nil, fmt.Errorf("analytics series exceeds %d buckets", model.MaxBuckets)
		}
		sequence = append(sequence, analyticsBucket{start: start, end: end})
		start = end
		_, end, err = aggregate.BucketBounds(start, query.TimeZone, width)
		if err != nil {
			return nil, err
		}
	}
	return sequence, nil
}

func (s *SQLiteStore) addActivityOutcomes(ctx context.Context, query model.Query, width string, buckets map[int64]*model.ActivityBucket) error {
	rawWhere, rawArguments, err := buildWhere(query)
	if err != nil {
		return err
	}
	rollupWhere, rollupArguments, err := buildRollupWhere(query, "bucket_start_ns", "bucket_end_ns")
	if err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return ErrClosed
	}
	rawRows, err := s.db.QueryContext(ctx, `SELECT requested_at_ns,succeeded FROM events `+rawWhere, rawArguments...)
	if err != nil {
		return fmt.Errorf("query analytics activity outcomes: %w", err)
	}
	for rawRows.Next() {
		var requestedNS int64
		var succeeded bool
		if err := rawRows.Scan(&requestedNS, &succeeded); err != nil {
			_ = rawRows.Close()
			return fmt.Errorf("scan analytics activity outcome: %w", err)
		}
		start, _, errBounds := aggregate.BucketBounds(time.Unix(0, requestedNS).UTC(), query.TimeZone, width)
		if errBounds != nil {
			_ = rawRows.Close()
			return errBounds
		}
		addActivityOutcome(buckets[start.UnixNano()], succeeded, 1)
	}
	if err := rawRows.Err(); err != nil {
		_ = rawRows.Close()
		return fmt.Errorf("read analytics activity outcomes: %w", err)
	}
	if err := rawRows.Close(); err != nil {
		return err
	}
	rollupRows, err := s.db.QueryContext(ctx, `SELECT grain,bucket_start_ns,succeeded,upstream_attempts FROM rollups `+rollupWhere, rollupArguments...)
	if err != nil {
		return fmt.Errorf("query retained analytics activity outcomes: %w", err)
	}
	defer func() { _ = rollupRows.Close() }()
	for rollupRows.Next() {
		var grain string
		var bucketNS, attempts int64
		var succeeded bool
		if err := rollupRows.Scan(&grain, &bucketNS, &succeeded, &attempts); err != nil {
			return fmt.Errorf("scan retained analytics activity outcome: %w", err)
		}
		if err := validateRollupWidth(grain, width); err != nil {
			return err
		}
		start, _, errBounds := aggregate.BucketBounds(time.Unix(0, bucketNS).UTC(), query.TimeZone, width)
		if errBounds != nil {
			return errBounds
		}
		addActivityOutcome(buckets[start.UnixNano()], succeeded, attempts)
	}
	return rollupRows.Err()
}

func addActivityOutcome(bucket *model.ActivityBucket, succeeded bool, attempts int64) {
	if bucket == nil {
		return
	}
	if succeeded {
		bucket.Succeeded += attempts
	} else {
		bucket.Failed += attempts
	}
}

func activityGrain(window string) (string, error) {
	switch window {
	case "day":
		return "5m", nil
	case "week", "month":
		return "1h", nil
	case "year":
		return "1d", nil
	default:
		return "", fmt.Errorf("unsupported activity window %q", window)
	}
}
