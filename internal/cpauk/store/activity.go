package store

import (
	"context"
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
	buckets, err := s.activityBuckets(ctx, query, grain)
	if err != nil {
		return model.Activity{}, err
	}
	return model.Activity{Meta: responseMeta(query), Grain: grain, Zone: query.TimeZone, Buckets: buckets}, nil
}

func (s *SQLiteStore) activityBuckets(ctx context.Context, query model.Query, width string) ([]model.ActivityBucket, error) {
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
