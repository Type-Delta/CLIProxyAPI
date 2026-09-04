package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	_ "modernc.org/sqlite"
)

func TestRetentionWritesDailyStatsFromRawEvents(t *testing.T) {
	const zone = "Asia/Kolkata"
	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	database := openRetainedCorrectnessStoreInZone(t, zone)
	fixture := loadFixtureEvents(t)[0]
	keyID := fmt.Sprintf("%064x", 7)
	day := time.Date(2026, 3, 8, 0, 0, 0, 0, location)
	events := []model.Event{
		yearActivityEvent(fixture, 1, 101, keyID, day.Add(2*time.Hour).UTC(), true, 10),
		yearActivityEvent(fixture, 2, 101, keyID, day.Add(3*time.Hour).UTC(), false, 20),
		yearActivityEvent(fixture, 3, 103, keyID, day.Add(4*time.Hour).UTC(), true, 30),
	}
	ctx := context.Background()
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ApplyRetention(ctx, day.AddDate(0, 0, 1).UTC(), 1); err != nil {
		t.Fatal(err)
	}
	var got dailyStatsRow
	var requests int64
	err = database.db.QueryRowContext(ctx, `SELECT day_end_ns,requests,succeeded,failed,input_tokens,
output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens
FROM daily_stats WHERE day_start_ns=? AND key_id=?`, day.UTC().UnixNano(), keyID).Scan(
		&got.dayEnd, &requests, &got.succeeded, &got.failed, &got.input, &got.output,
		&got.reasoning, &got.cached, &got.cacheRead, &got.cacheCreation, &got.total)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || got != (dailyStatsRow{
		dayEnd: day.AddDate(0, 0, 1).UTC().UnixNano(), succeeded: 2, failed: 1,
		input: 60, output: 63, reasoning: 66, cached: 69, cacheRead: 72,
		cacheCreation: 75, total: 405,
	}) {
		t.Fatalf("daily stats requests=%d row=%+v", requests, got)
	}
}

func TestMigrationBackfillsDailyStatsFromRollups(t *testing.T) {
	const zone = "America/St_Johns"
	ctx := context.Background()
	directory := t.TempDir()
	path := filepath.Join(directory, "analytics.db")
	config := Config{Path: path, MaxStorageBytes: 64 << 20, RetentionTimeZone: zone, PriceBook: fixturePriceBook()}
	database, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	fixture := loadFixtureEvents(t)[0]
	day := time.Date(2026, 3, 8, 0, 0, 0, 0, location)
	keyID := fmt.Sprintf("%064x", 9)
	events := []model.Event{
		yearActivityEvent(fixture, 1, 101, keyID, day.Add(90*time.Minute).UTC(), true, 10),
		yearActivityEvent(fixture, 2, 102, keyID, day.Add(5*time.Hour).UTC(), false, 20),
	}
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ApplyRetention(ctx, day.AddDate(0, 0, 1).UTC(), 100); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}

	direct, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := direct.ExecContext(ctx, "DROP TABLE daily_stats; DELETE FROM schema_migrations WHERE version=2"); err != nil {
		_ = direct.Close()
		t.Fatal(err)
	}
	if err := direct.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	var requests, succeeded, failed, total int64
	if err := database.db.QueryRowContext(ctx, `SELECT requests,succeeded,failed,total_tokens
FROM daily_stats WHERE day_start_ns=? AND key_id=?`, day.UTC().UnixNano(), keyID).Scan(
		&requests, &succeeded, &failed, &total); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || succeeded != 1 || failed != 1 || total != 210 {
		t.Fatalf("backfilled daily stats requests=%d succeeded=%d failed=%d total=%d", requests, succeeded, failed, total)
	}
	if database.SchemaVersion() != 2 {
		t.Fatalf("schema version=%d", database.SchemaVersion())
	}
}
