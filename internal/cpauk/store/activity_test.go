package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func TestYearActivityCombinesDailyStatsAndHotEvents(t *testing.T) {
	const zone = "America/St_Johns"
	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)
	oldAt := time.Date(2026, 4, 18, 12, 0, 0, 0, location).UTC()
	hotAt := time.Date(2026, 10, 28, 12, 0, 0, 0, location).UTC()
	keyA := fmt.Sprintf("%064x", 1)
	keyB := fmt.Sprintf("%064x", 2)
	fixture := loadFixtureEvents(t)[0]
	events := []model.Event{
		yearActivityEvent(fixture, 1, 101, keyA, oldAt, true, 100),
		yearActivityEvent(fixture, 2, 102, keyB, oldAt, false, 200),
		yearActivityEvent(fixture, 3, 103, keyA, hotAt, false, 300),
		yearActivityEvent(fixture, 4, 104, keyB, hotAt, true, 400),
	}
	database := openRetainedCorrectnessStoreInZone(t, zone)
	ctx := context.Background()
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ApplyRetention(ctx, oldAt.AddDate(0, 0, 2), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, "DELETE FROM rollups; DELETE FROM request_rollups"); err != nil {
		t.Fatal(err)
	}

	query := model.Query{SchemaVersion: 2, Operation: model.OperationActivity,
		Start: now.Add(-365 * 24 * time.Hour), End: now, TimeZone: zone, Window: "year", KeyIDs: []string{keyA}}
	activity, err := database.Activity(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if len(activity.Buckets) != 365 {
		t.Fatalf("activity returned %d buckets, want 365", len(activity.Buckets))
	}
	for index := 1; index < len(activity.Buckets); index++ {
		if !activity.Buckets[index-1].End.Equal(activity.Buckets[index].Start) {
			t.Fatalf("bucket %d is not adjacent: %+v then %+v", index, activity.Buckets[index-1], activity.Buckets[index])
		}
	}
	assertYearActivityBucket(t, activity.Buckets, oldAt, zone, model.ActivityBucket{
		Requests: 1, Succeeded: 1, InputTokens: 100, OutputTokens: 101, ReasoningTokens: 102,
		CachedTokens: 103, CacheReadTokens: 104, CacheCreationTokens: 105, TotalTokens: 615,
	})
	assertYearActivityBucket(t, activity.Buckets, hotAt, zone, model.ActivityBucket{
		Requests: 1, Failed: 1, InputTokens: 300, OutputTokens: 301, ReasoningTokens: 302,
		CachedTokens: 303, CacheReadTokens: 304, CacheCreationTokens: 305, TotalTokens: 1815,
	})
}

func TestYearActivityPreservesCostAfterRetention(t *testing.T) {
	const zone = "UTC"
	day := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	database := openRetainedCorrectnessStoreInZone(t, zone)
	fixture := loadFixtureEvents(t)[0]
	keyID := fmt.Sprintf("%064x", 7)
	events := []model.Event{
		yearActivityEvent(fixture, 1, 101, keyID, day.Add(2*time.Hour), true, 10),
		yearActivityEvent(fixture, 2, 102, keyID, day.Add(3*time.Hour), false, 20),
	}
	ctx := context.Background()
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	query := model.Query{SchemaVersion: 2, Operation: model.OperationActivity,
		Start: day, End: day.AddDate(0, 0, 1), TimeZone: zone, Window: "year"}
	before, err := database.Activity(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	beforeBucket, ok := activityBucketAt(before.Buckets, day)
	if !ok || beforeBucket.KnownCost == 0 {
		t.Fatalf("raw activity bucket = %+v, want known cost", before.Buckets)
	}

	if _, err := database.ApplyRetention(ctx, day.AddDate(0, 0, 1), 100); err != nil {
		t.Fatal(err)
	}
	after, err := database.Activity(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	afterBucket, ok := activityBucketAt(after.Buckets, day)
	if !ok || afterBucket.KnownCost != beforeBucket.KnownCost {
		t.Fatalf("retained activity cost = %v, want %v", afterBucket.KnownCost, beforeBucket.KnownCost)
	}
	var storedCost, storedUnpriced int64
	if err := database.db.QueryRowContext(ctx, "SELECT known_cost_nano,unpriced_tokens FROM daily_stats WHERE day_start_ns=? AND key_id=?", day.UnixNano(), keyID).Scan(&storedCost, &storedUnpriced); err != nil {
		t.Fatal(err)
	}
	if storedCost != int64(beforeBucket.KnownCost) || storedUnpriced != 148 {
		t.Fatalf("daily stats cost=%d unpriced=%d, want cost=%d unpriced=148", storedCost, storedUnpriced, beforeBucket.KnownCost)
	}
}

func TestYearActivityDeduplicatesRequestsAcrossRetainedBoundary(t *testing.T) {
	const zone = "UTC"
	day := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	database := openRetainedCorrectnessStoreInZone(t, zone)
	fixture := loadFixtureEvents(t)[0]
	keyID := fmt.Sprintf("%064x", 8)
	events := []model.Event{
		yearActivityEvent(fixture, 1, 101, keyID, day.Add(30*time.Minute), true, 10),
		yearActivityEvent(fixture, 2, 101, keyID, day.Add(90*time.Minute), false, 20),
		yearActivityEvent(fixture, 3, 102, keyID, day.Add(105*time.Minute), true, 30),
	}
	ctx := context.Background()
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	query := model.Query{SchemaVersion: 2, Operation: model.OperationActivity,
		Start: day, End: day.AddDate(0, 0, 1), TimeZone: zone, Window: "year"}
	before, err := database.Activity(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	beforeBucket, ok := activityBucketAt(before.Buckets, day)
	if !ok || beforeBucket.Requests != 2 || beforeBucket.Succeeded != 2 || beforeBucket.Failed != 1 {
		t.Fatalf("raw activity bucket = %+v", beforeBucket)
	}

	if _, err := database.ApplyRetention(ctx, day.Add(time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	after, err := database.Activity(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	afterBucket, ok := activityBucketAt(after.Buckets, day)
	if !ok || afterBucket.Requests != 2 || afterBucket.Succeeded != 2 || afterBucket.Failed != 1 {
		t.Fatalf("retained activity bucket = %+v, want two distinct requests", afterBucket)
	}
}

func TestYearActivityRejectsCrossZoneDailyStats(t *testing.T) {
	database := openRetainedCorrectnessStoreInZone(t, "America/St_Johns")
	fixture := loadFixtureEvents(t)[0]
	event := yearActivityEvent(fixture, 1, 101, fmt.Sprintf("%064x", 1),
		time.Date(2026, 4, 18, 15, 0, 0, 0, time.UTC), true, 100)
	ctx := context.Background()
	if err := database.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ApplyRetention(ctx, event.RequestedAt.Add(48*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	_, err := database.Activity(ctx, model.Query{SchemaVersion: 2, Operation: model.OperationActivity,
		Start: time.Date(2025, 11, 4, 15, 0, 0, 0, time.UTC), End: time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC),
		TimeZone: "Asia/Kolkata", Window: "year"})
	var zoneErr RetainedTimeZoneError
	if !errors.As(err, &zoneErr) || zoneErr.BucketWidth != "1d" {
		t.Fatalf("cross-zone year activity error = %v", err)
	}
}

func yearActivityEvent(fixture model.Event, attempt, request int, keyID string, at time.Time, succeeded bool, base int64) model.Event {
	event := fixture
	event.AttemptID = fmt.Sprintf("%032x", attempt)
	event.ProxyRequestID = fmt.Sprintf("%032x", request)
	event.KeyID = keyID
	event.RequestedAt = at
	event.Succeeded = succeeded
	if succeeded {
		event.UpstreamStatusCode, event.ErrorClass = nil, nil
	} else {
		status := 429
		errorClass := "rate_limit"
		event.UpstreamStatusCode, event.ErrorClass = &status, &errorClass
	}
	event.Tokens = model.TokenUsage{
		Input: base, Output: base + 1, Reasoning: base + 2, Cached: base + 3,
		CacheRead: base + 4, CacheCreation: base + 5, Total: 6*base + 15,
		Schema: "normalized-v1", Quality: model.TokenQualityExact,
	}
	return event
}

func assertYearActivityBucket(t *testing.T, buckets []model.ActivityBucket, at time.Time, zone string, want model.ActivityBucket) {
	t.Helper()
	start, _, err := aggregate.BucketBounds(at, zone, "1d")
	if err != nil {
		t.Fatal(err)
	}
	for _, bucket := range buckets {
		if !bucket.Start.Equal(start) {
			continue
		}
		bucket.Start, bucket.End, bucket.KnownCost, bucket.UnpricedTokens = time.Time{}, time.Time{}, 0, 0
		if bucket != want {
			t.Fatalf("activity bucket at %s = %+v, want %+v", start, bucket, want)
		}
		return
	}
	t.Fatalf("activity bucket at %s is missing", start)
}

func activityBucketAt(buckets []model.ActivityBucket, at time.Time) (model.ActivityBucket, bool) {
	start, _, err := aggregate.BucketBounds(at, "UTC", "1d")
	if err != nil {
		return model.ActivityBucket{}, false
	}
	for _, bucket := range buckets {
		if bucket.Start.Equal(start) {
			return bucket, true
		}
	}
	return model.ActivityBucket{}, false
}
