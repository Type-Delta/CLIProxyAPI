package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func TestHourlyAndDailyRetentionPreserveQueryParity(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, Config{
		Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20,
		PriceBook: fixturePriceBook(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	events := loadFixtureEvents(t)
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	query := model.Query{SchemaVersion: 1, Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		End: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), TimeZone: "UTC"}
	queries := []model.Query{query, query, query}
	queries[1].KeyIDs = []string{events[0].KeyID}
	queries[2].KeyIDs = []string{events[0].KeyID, events[1].KeyID}
	want := make([]retainedQuerySnapshot, len(queries))
	for index := range queries {
		want[index] = retainedSnapshot(t, database, queries[index], "1d")
	}
	wantLeaderboardOrder := pagedLeaderboardOrder(t, database, query)

	result, err := database.ApplyRetentionPolicy(ctx, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Time{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedRows != int64(len(events)) || result.RawCheckpoint == nil || !result.RawCheckpoint.Equal(events[len(events)-1].RequestedAt) {
		t.Fatalf("hourly retention result = %+v", result)
	}
	for index := range queries {
		assertRetainedSnapshot(t, database, queries[index], "1d", want[index])
	}
	if got := pagedLeaderboardOrder(t, database, query); !equalStrings(got, wantLeaderboardOrder) {
		t.Fatalf("hourly leaderboard order=%v want=%v", got, wantLeaderboardOrder)
	}

	result, err = database.ApplyRetentionPolicy(ctx, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedHourlyRows == 0 || result.HourlyCheckpoint == nil {
		t.Fatalf("daily retention result = %+v", result)
	}
	var hourly, daily int
	if err := database.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM rollups WHERE grain='hourly'").Scan(&hourly); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM rollups WHERE grain='daily'").Scan(&daily); err != nil {
		t.Fatal(err)
	}
	if hourly != 0 || daily == 0 {
		t.Fatalf("hourly=%d daily=%d", hourly, daily)
	}
	for index := range queries {
		assertRetainedSnapshot(t, database, queries[index], "1d", want[index])
	}
	if got := pagedLeaderboardOrder(t, database, query); !equalStrings(got, wantLeaderboardOrder) {
		t.Fatalf("daily leaderboard order=%v want=%v", got, wantLeaderboardOrder)
	}
}

type retainedQuerySnapshot struct {
	summary     model.Summary
	timeseries  model.Timeseries
	dimensions  model.DimensionPage
	leaderboard model.LeaderboardPage
}

func retainedSnapshot(t *testing.T, database *SQLiteStore, base model.Query, width string) retainedQuerySnapshot {
	t.Helper()
	base.Operation = model.OperationSummary
	summary, err := database.Summary(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	timeseriesQuery := base
	timeseriesQuery.Operation, timeseriesQuery.BucketWidth = model.OperationTimeseries, width
	timeseries, err := database.Timeseries(context.Background(), timeseriesQuery)
	if err != nil {
		t.Fatal(err)
	}
	dimensionsQuery := base
	dimensionsQuery.Operation, dimensionsQuery.Dimension, dimensionsQuery.PageSize = model.OperationDimensions, "provider", 100
	dimensions, err := database.Dimensions(context.Background(), dimensionsQuery)
	if err != nil {
		t.Fatal(err)
	}
	leaderboardQuery := base
	leaderboardQuery.Operation, leaderboardQuery.SortBy, leaderboardQuery.PageSize = model.OperationLeaderboard, model.LeaderboardSortCost, 100
	leaderboard, err := database.Leaderboard(context.Background(), leaderboardQuery)
	if err != nil {
		t.Fatal(err)
	}
	return retainedQuerySnapshot{summary: summary, timeseries: timeseries, dimensions: dimensions, leaderboard: leaderboard}
}

func assertRetainedSnapshot(t *testing.T, database *SQLiteStore, query model.Query, width string, want retainedQuerySnapshot) {
	t.Helper()
	got := retainedSnapshot(t, database, query, width)
	if got.summary.ProxyRequests != want.summary.ProxyRequests || got.summary.UpstreamAttempts != want.summary.UpstreamAttempts ||
		got.summary.Tokens != want.summary.Tokens || got.summary.KnownCost != want.summary.KnownCost || got.summary.UnpricedTokens != want.summary.UnpricedTokens {
		t.Fatalf("summary mismatch\n got: %+v\nwant: %+v", got.summary, want.summary)
	}
	if len(got.timeseries.Points) != len(want.timeseries.Points) {
		t.Fatalf("timeseries length=%d want=%d", len(got.timeseries.Points), len(want.timeseries.Points))
	}
	for index := range want.timeseries.Points {
		if got.timeseries.Points[index] != want.timeseries.Points[index] {
			t.Fatalf("timeseries[%d]\n got: %+v\nwant: %+v", index, got.timeseries.Points[index], want.timeseries.Points[index])
		}
	}
	if len(got.dimensions.Rows) != len(want.dimensions.Rows) {
		t.Fatalf("dimension length=%d want=%d", len(got.dimensions.Rows), len(want.dimensions.Rows))
	}
	for index := range want.dimensions.Rows {
		if got.dimensions.Rows[index] != want.dimensions.Rows[index] {
			t.Fatalf("dimensions[%d]\n got: %+v\nwant: %+v", index, got.dimensions.Rows[index], want.dimensions.Rows[index])
		}
	}
	if len(got.leaderboard.Rows) != len(want.leaderboard.Rows) {
		t.Fatalf("leaderboard length=%d want=%d", len(got.leaderboard.Rows), len(want.leaderboard.Rows))
	}
	for index := range want.leaderboard.Rows {
		if got.leaderboard.Rows[index] != want.leaderboard.Rows[index] {
			t.Fatalf("leaderboard[%d]\n got: %+v\nwant: %+v", index, got.leaderboard.Rows[index], want.leaderboard.Rows[index])
		}
	}
}

func pagedLeaderboardOrder(t *testing.T, database *SQLiteStore, base model.Query) []string {
	t.Helper()
	query := base
	query.Operation, query.SortBy, query.PageSize = model.OperationLeaderboard, model.LeaderboardSortTokens, 1
	var result []string
	for {
		page, err := database.Leaderboard(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Rows) != 1 {
			t.Fatalf("leaderboard page rows=%d", len(page.Rows))
		}
		result = append(result, page.Rows[0].KeyID)
		if page.Meta.NextCursor == "" {
			return result
		}
		query.Cursor = page.Meta.NextCursor
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestRetentionUsesCompleteBucketsAndDurableBatchCheckpoints(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	events := loadFixtureEvents(t)
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}

	rolled, deleted, checkpoint, err := database.rollRawBatch(ctx, time.Date(2026, 8, 31, 5, 0, 0, 0, time.UTC), 1)
	if err != nil {
		t.Fatal(err)
	}
	if rolled == 0 || deleted != 1 || checkpoint == nil || !checkpoint.Equal(events[0].RequestedAt) {
		t.Fatalf("batch rolled=%d deleted=%d checkpoint=%v", rolled, deleted, checkpoint)
	}
	var stored int64
	if err := database.db.QueryRowContext(ctx, "SELECT completed_cutoff_ns FROM retention_state WHERE grain='raw_checkpoint'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != events[0].RequestedAt.UnixNano() {
		t.Fatalf("checkpoint=%d want=%d", stored, events[0].RequestedAt.UnixNano())
	}
	eventQuery := model.Query{SchemaVersion: 1, Operation: model.OperationEvents,
		Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		TimeZone: "UTC", PageSize: 100}
	if _, err := database.Events(ctx, eventQuery); !errors.Is(err, ErrRetainedRangePartial) {
		t.Fatalf("interrupted-batch events error=%v", err)
	}

	result, err := database.ApplyRetention(ctx, time.Date(2026, 8, 31, 4, 30, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedRows != 0 {
		t.Fatalf("partial hour was retained: %+v", result)
	}
	query := model.Query{SchemaVersion: 1, Operation: model.OperationSummary,
		Start: time.Date(2026, 8, 31, 4, 30, 0, 0, time.UTC), End: time.Date(2026, 8, 31, 5, 0, 0, 0, time.UTC), TimeZone: "UTC"}
	if _, err := database.Summary(ctx, query); !errors.Is(err, ErrRetainedRangePartial) {
		t.Fatalf("partial retained range error = %v", err)
	}
}

func TestInterruptedRetentionCheckpointProtectsEventQueriesAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.db")
	config := Config{Path: path, MaxStorageBytes: 64 << 20}
	database, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	events := loadFixtureEvents(t)
	if err = database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	_, deleted, checkpoint, err := database.rollRawBatch(ctx, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 1)
	if err != nil || deleted != 1 || checkpoint == nil {
		t.Fatalf("interrupted raw batch deleted=%d checkpoint=%v err=%v", deleted, checkpoint, err)
	}
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = recordCutoffTx(ctx, tx, "raw_checkpoint", checkpoint.Add(-24*time.Hour).UnixNano()); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = database.Close(ctx); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	query := model.Query{SchemaVersion: 1, Operation: model.OperationEvents,
		Start: checkpoint.Add(-time.Hour), End: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), TimeZone: "UTC", PageSize: 100}
	if _, err = database.Events(ctx, query); !errors.Is(err, ErrRetainedRangePartial) {
		t.Fatalf("reopened interrupted-batch events error=%v", err)
	}
}

func TestEventQueriesRejectRetainedHistoryAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.db")
	config := Config{Path: path, MaxStorageBytes: 64 << 20}
	database, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	events := loadFixtureEvents(t)
	if err = database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if _, err = database.ApplyRetention(ctx, cutoff, 10); err != nil {
		t.Fatal(err)
	}
	query := model.Query{SchemaVersion: 1, Operation: model.OperationEvents,
		Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		TimeZone: "UTC", PageSize: 100}
	if _, err = database.Events(ctx, query); !errors.Is(err, ErrRetainedRangePartial) {
		t.Fatalf("retained events error=%v", err)
	}
	if _, _, err = database.EventByAttemptID(ctx, events[0].AttemptID, query); !errors.Is(err, ErrRetainedRangePartial) {
		t.Fatalf("retained event detail error=%v", err)
	}
	if err = database.Close(ctx); err != nil {
		t.Fatal(err)
	}

	database, err = Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	if retained := database.RetentionCutoff(); retained == nil || !retained.Equal(cutoff) {
		t.Fatalf("reopened retention cutoff=%v want=%v", retained, cutoff)
	}
	if _, err = database.Events(ctx, query); !errors.Is(err, ErrRetainedRangePartial) {
		t.Fatalf("reopened retained events error=%v", err)
	}
}

func TestRetainedEventErrorCarriesTheRetentionCutoff(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.db")
	database, err := Open(ctx, Config{Path: path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	events := loadFixtureEvents(t)
	if err = database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if _, err = database.ApplyRetention(ctx, cutoff, 10); err != nil {
		t.Fatal(err)
	}
	query := model.Query{SchemaVersion: 1, Operation: model.OperationEvents,
		Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		TimeZone: "UTC", PageSize: 100}
	_, err = database.Events(ctx, query)
	var rangeErr RetainedRangeError
	if !errors.As(err, &rangeErr) {
		t.Fatalf("events error = %v, want RetainedRangeError", err)
	}
	retained := database.RetentionCutoff()
	if retained == nil || !rangeErr.Cutoff.Equal(*retained) {
		t.Fatalf("cutoff = %v, want %v", rangeErr.Cutoff, retained)
	}
	if !errors.Is(err, ErrRetainedRangePartial) {
		t.Fatalf("typed error lost the sentinel: %v", err)
	}
	if !strings.Contains(rangeErr.Error(), rangeErr.Cutoff.UTC().Format(time.RFC3339)) {
		t.Fatalf("error text omits the cutoff: %s", rangeErr.Error())
	}
	if _, _, err = database.EventByAttemptID(ctx, events[0].AttemptID, query); !errors.As(err, &rangeErr) {
		t.Fatalf("event detail error = %v, want RetainedRangeError", err)
	}
}

func TestStorageZoneMismatchNamesBothZones(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.db")
	config := Config{Path: path, MaxStorageBytes: 64 << 20, RetentionTimeZone: "UTC"}
	database, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if err = database.WriteBatch(ctx, loadFixtureEvents(t)); err != nil {
		t.Fatal(err)
	}
	if _, err = database.ApplyRetention(ctx, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 10); err != nil {
		t.Fatal(err)
	}
	if err = database.Close(ctx); err != nil {
		t.Fatal(err)
	}
	config.RetentionTimeZone = "Asia/Kolkata"
	_, err = Open(ctx, config)
	var mismatch ZoneMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("reopen error = %v, want ZoneMismatchError", err)
	}
	if mismatch.Stored != "UTC" || mismatch.Configured != "Asia/Kolkata" {
		t.Fatalf("mismatch = %+v", mismatch)
	}
	if !mismatch.Permanent() || mismatch.Category() != "storage_time_zone" {
		t.Fatalf("classification = %t/%s", mismatch.Permanent(), mismatch.Category())
	}
}
