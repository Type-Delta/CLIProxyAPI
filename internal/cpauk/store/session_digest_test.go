package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestSessionDigestMigrationFreshDatabase(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	if database.SchemaVersion() != 15 {
		t.Fatalf("schema version = %d", database.SchemaVersion())
	}
	var columns int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('events') WHERE name='session_digest'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 1 {
		t.Fatal("fresh database is missing session_digest")
	}
}

func TestSessionDigestMigrationUpgradesV11Database(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.db")
	database, err := Open(ctx, Config{Path: path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	event := v2Event(fmt.Sprintf("%032x", 1), fmt.Sprintf("%032x", 2), strings.Repeat("a", 64), leaderboardStart, true, nil, 0, 0, 1, 2)
	event.SessionDigest = session("1")
	if err := database.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `ALTER TABLE pricing_manual_rules DROP COLUMN service_tier_prices; ALTER TABLE pricing_catalog_rules DROP COLUMN service_tier_prices; DROP INDEX IF EXISTS events_arrival_estimate_idx; ALTER TABLE events DROP COLUMN stream; ALTER TABLE events DROP COLUMN reasoning_effort; ALTER TABLE events DROP COLUMN output_timing; ALTER TABLE events DROP COLUMN session_digest`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version>=12`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = Open(ctx, Config{Path: path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	if database.SchemaVersion() != 15 {
		t.Fatalf("upgraded schema version = %d", database.SchemaVersion())
	}
	var withDigest, total int64
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(session_digest) FROM events`).Scan(&total, &withDigest); err != nil {
		t.Fatal(err)
	}
	if total != 1 || withDigest != 0 {
		t.Fatalf("upgraded rows total=%d with_digest=%d", total, withDigest)
	}
	page, err := database.Events(ctx, v2Query(model.OperationEvents, leaderboardStart.Add(-time.Minute), leaderboardStart.Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].SessionDigest != nil {
		t.Fatalf("upgraded event = %+v", page.Events)
	}
	if _, err := os.Stat(path + ".pre-migration-v12"); err != nil {
		t.Fatalf("pre-migration backup: %v", err)
	}
}
