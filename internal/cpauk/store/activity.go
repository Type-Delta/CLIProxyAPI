package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
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
	if split, ok := activityFrozenBoundary(query); ok {
		historyQuery := query
		historyQuery.End = split
		history, err := s.cachedActivity(ctx, historyQuery)
		if err != nil {
			return model.Activity{}, err
		}
		liveQuery := query
		liveQuery.Start = split
		liveBuckets, err := s.activityBuckets(ctx, liveQuery, grain)
		if err != nil {
			return model.Activity{}, err
		}
		buckets := append(history.Buckets, liveBuckets...)
		return model.Activity{Meta: responseMeta(query), Grain: grain, Zone: query.TimeZone, Buckets: buckets}, nil
	}
	if historyQueryIsHistorical(query) {
		return s.cachedActivity(ctx, query)
	}
	return s.activityUncached(ctx, query)
}

func (s *SQLiteStore) activityUncached(ctx context.Context, query model.Query) (model.Activity, error) {
	grain, err := activityGrain(query.Window)
	if err != nil {
		return model.Activity{}, err
	}
	buckets, err := s.activityBuckets(ctx, query, grain)
	if err != nil {
		return model.Activity{}, err
	}
	return model.Activity{Meta: responseMeta(query), Grain: grain, Zone: query.TimeZone, Buckets: buckets}, nil
}

// activityFrozenBoundary returns the start of the local calendar day containing
// the query end. Buckets before that boundary are immutable for the dashboard;
// only the current day must be read again as new usage arrives.
func activityFrozenBoundary(query model.Query) (time.Time, bool) {
	location, err := time.LoadLocation(query.TimeZone)
	if err != nil {
		return time.Time{}, false
	}
	boundary, err := activityDayStart(query.End, query.TimeZone)
	if err != nil {
		return time.Time{}, false
	}
	localEnd := query.End.In(location)
	localNow := time.Now().In(location)
	if localEnd.Year() != localNow.Year() || localEnd.YearDay() != localNow.YearDay() {
		return time.Time{}, false
	}
	return boundary, query.Start.Before(boundary) && boundary.Before(query.End)
}

func activityDayStart(at time.Time, zone string) (time.Time, error) {
	bucketer, err := aggregate.NewBucketer(zone, "1d")
	if err != nil {
		return time.Time{}, err
	}
	start, _, err := bucketer.Bounds(at)
	return start, err
}

func (s *SQLiteStore) activityBuckets(ctx context.Context, query model.Query, width string) ([]model.ActivityBucket, error) {
	if query.Window == "year" && width == "1d" {
		return s.yearActivityBuckets(ctx, query)
	}
	timeseriesQuery := query
	timeseriesQuery.Operation = model.OperationTimeseries
	timeseriesQuery.Window = ""
	timeseriesQuery.BucketWidth = width
	timeseries, outcomes, err := s.timeseriesWithOutcomes(ctx, timeseriesQuery)
	if err != nil {
		return nil, err
	}
	sequence, err := analyticsBucketSequence(query, width)
	if err != nil {
		return nil, err
	}
	return activityBucketsFromTimeseries(sequence, timeseries, outcomes), nil
}

func activityBucketsFromTimeseries(sequence []analyticsBucket, timeseries model.Timeseries, outcomes map[int64][2]int64) []model.ActivityBucket {
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
			Succeeded: outcomes[point.Start.UnixNano()][0], Failed: outcomes[point.Start.UnixNano()][1],
			InputTokens: point.Tokens.Input, OutputTokens: point.Tokens.Output,
			CachedTokens: point.Tokens.Cached, CacheReadTokens: point.Tokens.CacheRead,
			CacheCreationTokens: point.Tokens.CacheCreation, ReasoningTokens: point.Tokens.Reasoning,
			TotalTokens: point.Tokens.Total, KnownCost: point.KnownCost, UnpricedTokens: point.UnpricedTokens,
		}
	}
	return buckets
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
	if boundary, ok := s.activityHotBoundary(query); ok {
		return s.hybridYearActivityBuckets(ctx, query, boundary)
	}
	if !s.hasRetainedOverlap(query) {
		timeseriesQuery := query
		timeseriesQuery.Operation = model.OperationTimeseries
		timeseriesQuery.Window = ""
		timeseriesQuery.BucketWidth = "1d"
		timeseries, outcomes, err := s.timeseriesWithOutcomes(ctx, timeseriesQuery)
		if err != nil {
			return nil, err
		}
		sequence, err := analyticsBucketSequence(query, "1d")
		if err != nil {
			return nil, err
		}
		return activityBucketsFromTimeseries(sequence, timeseries, outcomes), nil
	}
	return s.yearActivityBucketsUncached(ctx, query)
}

func (s *SQLiteStore) hybridYearActivityBuckets(ctx context.Context, query model.Query, boundary time.Time) ([]model.ActivityBucket, error) {
	sequence, err := analyticsBucketSequence(query, "1d")
	if err != nil {
		return nil, err
	}
	buckets := make([]model.ActivityBucket, len(sequence))
	byStart := make(map[int64]*model.ActivityBucket, len(sequence))
	for index, bounds := range sequence {
		buckets[index] = model.ActivityBucket{Start: bounds.start, End: bounds.end}
		byStart[bounds.start.UnixNano()] = &buckets[index]
	}
	retainedQuery := query
	retainedQuery.End = boundary
	retained, err := s.yearActivityBucketsUncached(ctx, retainedQuery)
	if err != nil {
		return nil, err
	}
	for _, bucket := range retained {
		if target := byStart[bucket.Start.UnixNano()]; target != nil {
			*target = bucket
		}
	}
	hotQuery := query
	hotQuery.Start = boundary
	hotQuery.Operation = model.OperationTimeseries
	hotQuery.Window = ""
	hotQuery.BucketWidth = "1d"
	timeseries, outcomes, err := s.timeseriesWithOutcomes(ctx, hotQuery)
	if err != nil {
		return nil, err
	}
	hotSequence, err := analyticsBucketSequence(hotQuery, "1d")
	if err != nil {
		return nil, err
	}
	for _, bucket := range activityBucketsFromTimeseries(hotSequence, timeseries, outcomes) {
		target := byStart[bucket.Start.UnixNano()]
		if target == nil {
			continue
		}
		target.Requests += bucket.Requests
		target.Succeeded += bucket.Succeeded
		target.Failed += bucket.Failed
		target.InputTokens += bucket.InputTokens
		target.OutputTokens += bucket.OutputTokens
		target.CachedTokens += bucket.CachedTokens
		target.CacheReadTokens += bucket.CacheReadTokens
		target.CacheCreationTokens += bucket.CacheCreationTokens
		target.ReasoningTokens += bucket.ReasoningTokens
		target.TotalTokens += bucket.TotalTokens
		target.KnownCost += bucket.KnownCost
		target.UnpricedTokens += bucket.UnpricedTokens
	}
	requestCounts, err := s.hybridActivityRequestCounts(ctx, query, boundary)
	if err != nil {
		return nil, err
	}
	for start, requests := range requestCounts {
		if target := byStart[start]; target != nil {
			target.Requests = int64(len(requests))
		}
	}
	return buckets, nil
}

func (s *SQLiteStore) hybridActivityRequestCounts(ctx context.Context, query model.Query, boundary time.Time) (map[int64]map[string]struct{}, error) {
	counts := make(map[int64]map[string]struct{})
	bucketer, err := aggregate.NewBucketer(query.TimeZone, "1d")
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrClosed
	}
	retainedQuery := query
	retainedQuery.End = boundary
	retainedWhere, retainedArguments, err := buildRollupWhere(retainedQuery, "bucket_start_ns", "bucket_end_ns")
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT bucket_start_ns,proxy_request_id FROM request_rollups `+retainedWhere, retainedArguments...)
	if err != nil {
		return nil, fmt.Errorf("query retained year activity request identities: %w", err)
	}
	for rows.Next() {
		var bucketStart int64
		var requestID string
		if err := rows.Scan(&bucketStart, &requestID); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan retained year activity request identity: %w", err)
		}
		start, _, err := bucketer.Bounds(time.Unix(0, bucketStart).UTC())
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		if counts[start.UnixNano()] == nil {
			counts[start.UnixNano()] = make(map[string]struct{})
		}
		counts[start.UnixNano()][requestID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read retained year activity request identities: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	hotWhere, hotArguments, err := buildWhere(query)
	if err != nil {
		return nil, err
	}
	hotArguments = append(hotArguments, boundary.UnixNano())
	hotWhere += " AND requested_at_ns >= ?"
	rawRows, err := s.db.QueryContext(ctx, `SELECT requested_at_ns,proxy_request_id FROM events `+hotWhere, hotArguments...)
	if err != nil {
		return nil, fmt.Errorf("query hot year activity request identities: %w", err)
	}
	for rawRows.Next() {
		var requestedNS int64
		var requestID string
		if err := rawRows.Scan(&requestedNS, &requestID); err != nil {
			_ = rawRows.Close()
			return nil, fmt.Errorf("scan hot year activity request identity: %w", err)
		}
		start, _, err := bucketer.Bounds(time.Unix(0, requestedNS).UTC())
		if err != nil {
			_ = rawRows.Close()
			return nil, err
		}
		if counts[start.UnixNano()] == nil {
			counts[start.UnixNano()] = make(map[string]struct{})
		}
		counts[start.UnixNano()][requestID] = struct{}{}
	}
	if err := rawRows.Err(); err != nil {
		_ = rawRows.Close()
		return nil, fmt.Errorf("read hot year activity request identities: %w", err)
	}
	if err := rawRows.Close(); err != nil {
		return nil, err
	}
	return counts, nil
}

func (s *SQLiteStore) activityHotBoundary(query model.Query) (time.Time, bool) {
	s.mu.RLock()
	cutoff := s.retentionCutoff
	s.mu.RUnlock()
	if cutoff.IsZero() || !query.Start.Before(cutoff) {
		return time.Time{}, false
	}
	boundary, err := activityDayStart(cutoff, query.TimeZone)
	if err != nil || !cutoff.Equal(boundary) {
		return time.Time{}, false
	}
	return boundary, boundary.Before(query.End)
}

func (s *SQLiteStore) yearActivityBucketsUncached(ctx context.Context, query model.Query) ([]model.ActivityBucket, error) {
	sequence, err := analyticsBucketSequence(query, "1d")
	if err != nil {
		return nil, err
	}
	buckets := make([]model.ActivityBucket, len(sequence))
	byStart := make(map[int64]*model.ActivityBucket, len(sequence))
	requests := make(map[int64]map[string]struct{}, len(sequence))
	retainedRequestCounts := make(map[int64]int64, len(sequence))
	retainedRequestDays := make(map[int64]bool, len(sequence))
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
	bucketer, err := aggregate.NewBucketer(query.TimeZone, "1d")
	if err != nil {
		return nil, err
	}
	rawRows, err := s.db.QueryContext(ctx, `SELECT requested_at_ns,proxy_request_id,succeeded,
input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,
cache_creation_tokens,total_tokens,known_cost_nano,unpriced_tokens FROM events `+rawWhere, rawArguments...)
	if err != nil {
		return nil, fmt.Errorf("query year activity events: %w", err)
	}
	for rawRows.Next() {
		var requestedNS int64
		var requestID string
		var succeeded bool
		var input, output, reasoning, cached, cacheRead, cacheCreation, total, unpriced int64
		var knownCost sql.NullInt64
		if err := rawRows.Scan(&requestedNS, &requestID, &succeeded, &input, &output, &reasoning,
			&cached, &cacheRead, &cacheCreation, &total, &knownCost, &unpriced); err != nil {
			_ = rawRows.Close()
			return nil, fmt.Errorf("scan year activity event: %w", err)
		}
		start, _, errBounds := bucketer.Bounds(time.Unix(0, requestedNS).UTC())
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
		addActivityTokens(bucket, input, output, reasoning, cached, cacheRead, cacheCreation, total, unpriced)
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
	statsWhere, statsArguments := dailyStatsWhere(query, false)
	statsRows, err := s.db.QueryContext(ctx, `SELECT day_start_ns,requests,succeeded,failed,
input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,
cache_creation_tokens,total_tokens,known_cost_nano,unpriced_tokens FROM daily_stats `+statsWhere, statsArguments...)
	if err != nil {
		return nil, fmt.Errorf("query retained year activity: %w", err)
	}
	for statsRows.Next() {
		var dayStart, requestCount, succeeded, failed int64
		var input, output, reasoning, cached, cacheRead, cacheCreation, total, knownCost, unpriced int64
		if err := statsRows.Scan(&dayStart, &requestCount, &succeeded, &failed, &input, &output,
			&reasoning, &cached, &cacheRead, &cacheCreation, &total, &knownCost, &unpriced); err != nil {
			return nil, fmt.Errorf("scan retained year activity: %w", err)
		}
		bucket := byStart[dayStart]
		if bucket == nil {
			continue
		}
		bucket.Succeeded += succeeded
		bucket.Failed += failed
		addActivityTokens(bucket, input, output, reasoning, cached, cacheRead, cacheCreation, total, unpriced)
		bucket.KnownCost += model.NanoUSD(knownCost)
		retainedRequestCounts[dayStart] += requestCount
	}
	if err := statsRows.Err(); err != nil {
		return nil, fmt.Errorf("read retained year activity: %w", err)
	}
	if err := statsRows.Close(); err != nil {
		return nil, fmt.Errorf("close retained year activity: %w", err)
	}

	retainedWhere, retainedArguments, err := buildRollupWhere(query, "bucket_start_ns", "bucket_end_ns")
	if err != nil {
		return nil, err
	}
	requestRows, err := s.db.QueryContext(ctx, `SELECT DISTINCT bucket_start_ns,proxy_request_id FROM request_rollups `+retainedWhere, retainedArguments...)
	if err != nil {
		return nil, fmt.Errorf("query retained year activity requests: %w", err)
	}
	for requestRows.Next() {
		var bucketStart int64
		var requestID string
		if err := requestRows.Scan(&bucketStart, &requestID); err != nil {
			_ = requestRows.Close()
			return nil, fmt.Errorf("scan retained year activity request: %w", err)
		}
		start, _, errBounds := bucketer.Bounds(time.Unix(0, bucketStart).UTC())
		if errBounds != nil {
			_ = requestRows.Close()
			return nil, errBounds
		}
		if byStart[start.UnixNano()] == nil {
			continue
		}
		if requests[start.UnixNano()] == nil {
			requests[start.UnixNano()] = map[string]struct{}{}
		}
		requests[start.UnixNano()][requestID] = struct{}{}
		retainedRequestDays[start.UnixNano()] = true
	}
	if err := requestRows.Err(); err != nil {
		_ = requestRows.Close()
		return nil, fmt.Errorf("read retained year activity requests: %w", err)
	}
	if err := requestRows.Close(); err != nil {
		return nil, fmt.Errorf("close retained year activity requests: %w", err)
	}
	for start, values := range requests {
		byStart[start].Requests = int64(len(values))
	}
	for start, count := range retainedRequestCounts {
		if !retainedRequestDays[start] {
			byStart[start].Requests += count
		}
	}
	return buckets, nil
}

func (s *SQLiteStore) hasRetainedOverlap(query model.Query) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.retentionCutoff.IsZero() && query.Start.Before(s.retentionCutoff)
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

func addActivityTokens(bucket *model.ActivityBucket, input, output, reasoning, cached, cacheRead, cacheCreation, total, unpriced int64) {
	bucket.InputTokens += input
	bucket.OutputTokens += output
	bucket.ReasoningTokens += reasoning
	bucket.CachedTokens += cached
	bucket.CacheReadTokens += cacheRead
	bucket.CacheCreationTokens += cacheCreation
	bucket.TotalTokens += total
	bucket.UnpricedTokens += unpriced
}

type analyticsBucket struct {
	start time.Time
	end   time.Time
}

func analyticsBucketSequence(query model.Query, width string) ([]analyticsBucket, error) {
	bucketer, err := aggregate.NewBucketer(query.TimeZone, width)
	if err != nil {
		return nil, err
	}
	start, end, err := bucketer.Bounds(query.Start)
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
		_, end, err = bucketer.Bounds(start)
		if err != nil {
			return nil, err
		}
	}
	return sequence, nil
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
