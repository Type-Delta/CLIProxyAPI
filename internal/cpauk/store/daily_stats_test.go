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
output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,
known_cost_nano,unpriced_tokens
FROM daily_stats WHERE day_start_ns=? AND key_id=?`, day.UTC().UnixNano(), keyID).Scan(
		&got.dayEnd, &requests, &got.succeeded, &got.failed, &got.input, &got.output,
		&got.reasoning, &got.cached, &got.cacheRead, &got.cacheCreation, &got.total, &got.knownCost, &got.unpriced)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || got.succeeded != 2 || got.failed != 1 || got.input != 60 || got.output != 63 || got.reasoning != 66 ||
		got.cached != 69 || got.cacheRead != 72 || got.cacheCreation != 75 || got.total != 405 || got.knownCost <= 0 || got.unpriced != 282 {
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
	if _, err := direct.ExecContext(ctx, "ALTER TABLE events DROP COLUMN routing_time_ms; ALTER TABLE events DROP COLUMN first_token_latency_ms; ALTER TABLE events DROP COLUMN provider_latency_ms; ALTER TABLE events DROP COLUMN client_method; ALTER TABLE events DROP COLUMN client_path; ALTER TABLE events DROP COLUMN received_at_ns; ALTER TABLE events DROP COLUMN upstream_method; ALTER TABLE events DROP COLUMN upstream_url; ALTER TABLE events DROP COLUMN upstream_sent_at_ns; ALTER TABLE events DROP COLUMN upstream_usage_raw; ALTER TABLE events DROP COLUMN upstream_error_body; ALTER TABLE events DROP COLUMN proxy_status_code; ALTER TABLE events DROP COLUMN proxy_error; ALTER TABLE events DROP COLUMN responded_at_ns; ALTER TABLE events DROP COLUMN generation_time_ms; DROP TABLE daily_stats; DELETE FROM schema_migrations WHERE version>=2"); err != nil {
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
	if database.SchemaVersion() != 9 {
		t.Fatalf("schema version=%d", database.SchemaVersion())
	}
}

func TestMigrationFiveBackfillsDailyStatsCost(t *testing.T) {
	const zone = "UTC"
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.db")
	config := Config{Path: path, MaxStorageBytes: 64 << 20, RetentionTimeZone: zone, PriceBook: fixturePriceBook()}
	database, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	fixture := loadFixtureEvents(t)[0]
	day := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)
	keyID := fmt.Sprintf("%064x", 9)
	events := []model.Event{
		yearActivityEvent(fixture, 1, 101, keyID, day.Add(90*time.Minute), true, 10),
		yearActivityEvent(fixture, 2, 102, keyID, day.Add(5*time.Hour), false, 20),
	}
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ApplyRetention(ctx, day.AddDate(0, 0, 1), 100); err != nil {
		t.Fatal(err)
	}
	var wantCost int64
	if err := database.db.QueryRowContext(ctx, `SELECT known_cost_nano FROM daily_stats WHERE day_start_ns=? AND key_id=?`,
		day.UnixNano(), keyID).Scan(&wantCost); err != nil {
		t.Fatal(err)
	}
	if wantCost == 0 {
		t.Fatal("fixture produced no known cost; the backfill assertion would be vacuous")
	}
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// Rewind to schema version 4: a daily_stats table that never carried cost.
	direct, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := direct.ExecContext(ctx, `ALTER TABLE events DROP COLUMN routing_time_ms; ALTER TABLE events DROP COLUMN first_token_latency_ms; ALTER TABLE events DROP COLUMN provider_latency_ms; ALTER TABLE events DROP COLUMN client_method; ALTER TABLE events DROP COLUMN client_path; ALTER TABLE events DROP COLUMN received_at_ns; ALTER TABLE events DROP COLUMN upstream_method; ALTER TABLE events DROP COLUMN upstream_url; ALTER TABLE events DROP COLUMN upstream_sent_at_ns; ALTER TABLE events DROP COLUMN upstream_usage_raw; ALTER TABLE events DROP COLUMN upstream_error_body; ALTER TABLE events DROP COLUMN proxy_status_code; ALTER TABLE events DROP COLUMN proxy_error; ALTER TABLE events DROP COLUMN responded_at_ns; ALTER TABLE daily_stats DROP COLUMN known_cost_nano;
ALTER TABLE daily_stats DROP COLUMN unpriced_tokens; DELETE FROM schema_migrations WHERE version>=5`); err != nil {
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
	var gotCost int64
	if err := database.db.QueryRowContext(ctx, `SELECT known_cost_nano FROM daily_stats WHERE day_start_ns=? AND key_id=?`,
		day.UnixNano(), keyID).Scan(&gotCost); err != nil {
		t.Fatal(err)
	}
	if gotCost != wantCost {
		t.Fatalf("migration 5 backfilled known_cost_nano=%d, want %d", gotCost, wantCost)
	}
}
