package chstore

import (
	"context"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func (s *Store) Activity(ctx context.Context, q model.Query) (model.Activity, error) {
	if err := s.validateQuery(&q, model.OperationActivity); err != nil {
		return model.Activity{}, err
	}
	grain, err := activityGrain(q.Window)
	if err != nil {
		return model.Activity{}, err
	}
	q, err = normalizeYearActivityQuery(q, grain)
	if err != nil {
		return model.Activity{}, err
	}
	boundary, err := activityDayStart(s.now(), q.TimeZone)
	if err != nil {
		return model.Activity{}, err
	}
	if q.Start.Before(boundary) && boundary.Before(q.End) {
		historyQuery := q
		historyQuery.End = boundary
		history, err := s.cachedActivity(ctx, historyQuery, grain)
		if err != nil {
			return model.Activity{}, err
		}
		liveQuery := q
		liveQuery.Start = boundary
		live, err := s.activityBuckets(ctx, liveQuery, grain)
		if err != nil {
			return model.Activity{}, err
		}
		return model.Activity{Meta: responseMeta(q), Grain: grain, Zone: q.TimeZone, Buckets: append(history.Buckets, live...)}, nil
	}
	return s.cachedActivity(ctx, q, grain)
}

func (s *Store) cachedActivity(ctx context.Context, q model.Query, grain string) (model.Activity, error) {
	value, err := cachedHistory(s, ctx, q, func() (backend.HistoryCacheValue, error) {
		buckets, err := s.activityBuckets(ctx, q, grain)
		return backend.HistoryCacheValue{Operation: model.OperationActivity, Activity: model.Activity{Meta: responseMeta(q), Grain: grain, Zone: q.TimeZone, Buckets: buckets}}, err
	})
	return value.Activity, err
}
func activityDayStart(at time.Time, zone string) (time.Time, error) {
	b, err := aggregate.NewBucketer(zone, "1d")
	if err != nil {
		return time.Time{}, err
	}
	start, _, err := b.Bounds(at)
	return start, err
}
func (s *Store) activityBuckets(ctx context.Context, q model.Query, width string) ([]model.ActivityBucket, error) {
	if q.Window == "year" && width == "1d" {
		s.mu.RLock()
		if s.db == nil {
			s.mu.RUnlock()
			return nil, ErrClosed
		}
		err := s.validateYearActivity(ctx, q)
		s.mu.RUnlock()
		if err != nil {
			return nil, err
		}
	}
	tq := q
	tq.Operation = model.OperationTimeseries
	tq.Window = ""
	tq.BucketWidth = width
	series, outcomes, err := s.timeseriesWithOutcomes(ctx, tq)
	if err != nil {
		return nil, err
	}
	sequence, err := analyticsBucketSequence(q, width)
	if err != nil {
		return nil, err
	}
	return activityBucketsFromTimeseries(sequence, series, outcomes), nil
}

// Year activity preserves the legacy daily-stat validation even though its
// values now come from published accounting and exact request facts.
func (s *Store) validateYearActivity(ctx context.Context, q model.Query) error {
	ctx = withReadCutoffs(ctx)
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
	all := q
	all.Start = dayStart
	all.Filters = nil
	retained, err := s.retainedRangeSelection(ctx, all, false, true, "bucket_start_ns")
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
