package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	_ "modernc.org/sqlite"
)

func TestMaterialized15MinuteIncrementalUpsertIsDuplicateSafe(t *testing.T) {
	ctx := context.Background()
	database := openMaterialized15mTestStore(t)
	base := loadFixtureEvents(t)[0]
	base.RequestedAt = time.Date(2026, 10, 7, 12, 14, 59, 0, time.UTC)
	base.AttemptID = fmt.Sprintf("%032x", 1)
	base.ProxyRequestID = fmt.Sprintf("%032x", 7)
	base.KeyID = fmt.Sprintf("%064x", 11)
	base.Tokens.Input = 10
	base.Tokens.Total = 20

	second := base
	second.AttemptID = fmt.Sprintf("%032x", 2)
	second.Tokens.Input = 30
	second.Tokens.Total = 40
	if err := database.WriteBatch(ctx, []model.Event{base, base, second}); err != nil {
		t.Fatal(err)
	}
	start := base.RequestedAt.Truncate(materialized15mDuration).UnixNano()
	var rows, requests, attempts, input, total int64
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*), proxy_requests, upstream_attempts,
input_tokens, total_tokens FROM analytics_15m_compact WHERE bucket_start_ns=? AND dimension_kind='overall'`, start).Scan(
		&rows, &requests, &attempts, &input, &total); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || requests != 1 || attempts != 2 || input != 40 || total != 60 {
		t.Fatalf("aggregate rows=%d requests=%d attempts=%d input=%d total=%d", rows, requests, attempts, input, total)
	}
	var identityRows int64
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM analytics_15m_request_ids
WHERE bucket_start_ns=?`, start).Scan(&identityRows); err != nil {
		t.Fatal(err)
	}
	if identityRows != 1 {
		t.Fatalf("request identities=%d, want 1", identityRows)
	}
	if err := database.WriteBatch(ctx, []model.Event{base}); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, `SELECT upstream_attempts, input_tokens
FROM analytics_15m_compact WHERE bucket_start_ns=? AND dimension_kind='overall'`, start).Scan(&attempts, &input); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || input != 40 {
		t.Fatalf("duplicate write changed aggregate attempts=%d input=%d", attempts, input)
	}

	boundary := base
	boundary.AttemptID = fmt.Sprintf("%032x", 3)
	boundary.ProxyRequestID = fmt.Sprintf("%032x", 8)
	boundary.RequestedAt = base.RequestedAt.Add(time.Second)
	if err := database.WriteBatch(ctx, []model.Event{boundary}); err != nil {
		t.Fatal(err)
	}
	var bucketRows int64
	if err := database.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM analytics_15m_compact WHERE dimension_kind='overall'").Scan(&bucketRows); err != nil {
		t.Fatal(err)
	}
	if bucketRows != 2 {
		t.Fatalf("bucket rows=%d, want 2 across a 15-minute boundary", bucketRows)
	}
}

func TestRebuild15MinuteAggregatesBackfillsEvents(t *testing.T) {
	ctx := context.Background()
	database := openMaterialized15mTestStore(t)
	events := loadFixtureEvents(t)[:2]
	for index := range events {
		events[index].RequestedAt = time.Date(2026, 10, 7, 13, 0, index, 0, time.UTC)
		events[index].AttemptID = fmt.Sprintf("%032x", index+21)
		events[index].ProxyRequestID = fmt.Sprintf("%032x", index+31)
		events[index].KeyID = fmt.Sprintf("%064x", index+41)
	}
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, "DELETE FROM analytics_15m_compact; DELETE FROM analytics_15m_request_ids"); err != nil {
		t.Fatal(err)
	}
	if err := database.Rebuild15MinuteAggregates(ctx); err != nil {
		t.Fatal(err)
	}
	var aggregateRows, requestRows, attempts int64
	if err := database.db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(upstream_attempts),0) FROM analytics_15m_compact WHERE dimension_kind='overall'").Scan(&aggregateRows, &attempts); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM analytics_15m_request_ids").Scan(&requestRows); err != nil {
		t.Fatal(err)
	}
	if aggregateRows != 1 || requestRows != 2 || attempts != 2 {
		t.Fatalf("rebuilt aggregates=%d requests=%d attempts=%d", aggregateRows, requestRows, attempts)
	}
}

func TestMigrationBackfillsMaterialized15MinuteAggregates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.db")
	config := Config{Path: path, MaxStorageBytes: 64 << 20, PriceBook: fixturePriceBook()}
	database, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	event := loadFixtureEvents(t)[0]
	event.RequestedAt = time.Date(2026, 10, 7, 14, 1, 0, 0, time.UTC)
	event.AttemptID = fmt.Sprintf("%032x", 51)
	event.ProxyRequestID = fmt.Sprintf("%032x", 61)
	if err := database.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}

	direct, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := direct.ExecContext(ctx, "DROP TABLE IF EXISTS analytics_15m_request_ids; DROP TABLE IF EXISTS analytics_15m_compact; DROP TABLE IF EXISTS analytics_15m_requests; DROP TABLE IF EXISTS analytics_15m; DELETE FROM schema_migrations WHERE version >= 10"); err != nil {
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
	var aggregateRows, requestRows int64
	if err := database.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM analytics_15m_compact WHERE dimension_kind='overall'").Scan(&aggregateRows); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM analytics_15m_request_ids").Scan(&requestRows); err != nil {
		t.Fatal(err)
	}
	if aggregateRows != 1 || requestRows != 1 {
		t.Fatalf("migration backfill aggregates=%d requests=%d", aggregateRows, requestRows)
	}
}

func TestCompactTimeseriesAndDimensionsMatchRawAcrossWidthsAndFilters(t *testing.T) {
	ctx := context.Background()
	database := openMaterialized15mTestStore(t)
	fixture := loadFixtureEvents(t)[0]
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	keyA := fmt.Sprintf("%064x", 71)
	keyB := fmt.Sprintf("%064x", 72)
	events := make([]model.Event, 0, 4)
	for index, offset := range []time.Duration{time.Minute, 16 * time.Minute, 31 * time.Minute, 46 * time.Minute} {
		event := fixture
		event.AttemptID = fmt.Sprintf("%032x", 100+index)
		event.ProxyRequestID = fmt.Sprintf("%032x", 200+index/2)
		event.KeyID = keyA
		event.Provider = "compact-provider"
		event.Model = "compact-model-a"
		event.RequestedAt = start.Add(offset)
		event.Tokens.Input = int64(10 + index)
		event.Tokens.Output = int64(20 + index)
		event.Tokens.Total = event.Tokens.Input + event.Tokens.Output
		if index == 3 {
			event.KeyID = keyB
			event.Model = "compact-model-b"
			event.Provider = "compact-provider-b"
		}
		events = append(events, event)
	}
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	for _, width := range []string{"15m", "30m", "1h"} {
		for _, selectedKeys := range [][]string{nil, {keyA}} {
			compactQuery := model.Query{SchemaVersion: model.QuerySchemaVersionV2, Operation: model.OperationTimeseries,
				Start: start, End: start.Add(time.Hour), TimeZone: "UTC", BucketWidth: width, KeyIDs: selectedKeys}
			rawQuery := compactQuery
			rawQuery.TimeZone = "Etc/UTC"
			got, err := database.Timeseries(ctx, compactQuery)
			if err != nil {
				t.Fatalf("compact %s keys=%v: %v", width, selectedKeys != nil, err)
			}
			want, err := database.Timeseries(ctx, rawQuery)
			if err != nil {
				t.Fatalf("raw %s keys=%v: %v", width, selectedKeys != nil, err)
			}
			if !reflect.DeepEqual(got.Points, want.Points) {
				t.Fatalf("timeseries %s keys=%v differs\ncompact=%+v\nraw=%+v", width, selectedKeys != nil, got.Points, want.Points)
			}
		}
	}
	providerFilter, _ := json.Marshal([]string{"compact-provider"})
	compactProviderQuery := model.Query{SchemaVersion: model.QuerySchemaVersionV2, Operation: model.OperationTimeseries,
		Start: start, End: start.Add(time.Hour), TimeZone: "UTC", BucketWidth: "1h",
		Filters: map[string]json.RawMessage{"provider": providerFilter}}
	rawProviderQuery := compactProviderQuery
	rawProviderQuery.TimeZone = "Etc/UTC"
	compactProvider, err := database.Timeseries(ctx, compactProviderQuery)
	if err != nil {
		t.Fatal(err)
	}
	rawProvider, err := database.Timeseries(ctx, rawProviderQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(compactProvider.Points, rawProvider.Points) {
		t.Fatalf("provider filter differs\ncompact=%+v\nraw=%+v", compactProvider.Points, rawProvider.Points)
	}
	for _, dimension := range []string{"provider", "model", "key"} {
		compactQuery := model.Query{SchemaVersion: model.QuerySchemaVersionV2, Operation: model.OperationDimensions,
			Start: start, End: start.Add(time.Hour), TimeZone: "UTC", Dimension: dimension, PageSize: 20}
		rawQuery := compactQuery
		rawQuery.TimeZone = "Etc/UTC"
		got, err := database.Dimensions(ctx, compactQuery)
		if err != nil {
			t.Fatalf("compact dimensions %s: %v", dimension, err)
		}
		want, err := database.Dimensions(ctx, rawQuery)
		if err != nil {
			t.Fatalf("raw dimensions %s: %v", dimension, err)
		}
		if !reflect.DeepEqual(got.Rows, want.Rows) {
			t.Fatalf("dimensions %s differs\ncompact=%+v\nraw=%+v", dimension, got.Rows, want.Rows)
		}
	}
}

func openMaterialized15mTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	database, err := Open(context.Background(), Config{
		Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20,
		PriceBook: fixturePriceBook(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	return database
}
