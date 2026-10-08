package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func reviewEvent(t *testing.T, base model.Event, index int, at time.Time, key, batch string) model.Event {
	t.Helper()
	event := base
	event.AttemptID = fmt.Sprintf("%032x", 9000+index)
	event.ProxyRequestID = fmt.Sprintf("%032x", 19000+index)
	event.RequestedAt = at
	event.KeyID = key
	event.Provider = "review-provider"
	event.Model = "review-model"
	event.Tokens.Input = int64(index + 10)
	event.Tokens.Output = int64(index + 20)
	event.Tokens.Total = event.Tokens.Input + event.Tokens.Output
	if batch != "" {
		event.ImportBatchID = batch
	}
	return event
}

func TestCompactReviewMigrationFromSchemaTenPreservesExactRead(t *testing.T) {
	ctx := context.Background()
	config := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: fixturePriceBook()}
	database, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	base := loadFixtureEvents(t)[0]
	events := []model.Event{reviewEvent(t, base, 200, start.Add(time.Minute), fmt.Sprintf("%064x", 201), ""), reviewEvent(t, base, 201, start.Add(16*time.Minute), fmt.Sprintf("%064x", 202), "")}
	events[1].ProxyRequestID = events[0].ProxyRequestID
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	query := model.Query{SchemaVersion: 2, Operation: model.OperationTimeseries, Start: start, End: start.Add(24 * time.Hour), TimeZone: "Etc/UTC", BucketWidth: "1d"}
	want, err := database.Timeseries(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}
	direct, err := sql.Open("sqlite", config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := direct.ExecContext(ctx, `DROP TABLE analytics_15m_compact; DROP TABLE analytics_15m_request_ids; DELETE FROM schema_migrations WHERE version=11`); err != nil {
		t.Fatal(err)
	}
	legacySQL, err := migrationFiles.ReadFile("migrations/010_materialized_15m.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := direct.ExecContext(ctx, string(legacySQL)); err != nil {
		t.Fatal(err)
	}
	if err := direct.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(ctx) })
	query.TimeZone = "UTC"
	got, err := database.Timeseries(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Points, want.Points) {
		t.Fatalf("schema10 migration differs\ngot=%+v\nwant=%+v", got.Points, want.Points)
	}
	var legacyTables int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name IN ('analytics_15m','analytics_15m_requests')`).Scan(&legacyTables); err != nil || legacyTables != 0 {
		t.Fatalf("legacy tables=%d err=%v", legacyTables, err)
	}
}

func TestCompactReviewDimensionsLongRangeAndWeekMatchRaw(t *testing.T) {
	ctx := context.Background()
	database := openMaterialized15mTestStore(t)
	base := loadFixtureEvents(t)[0]
	start := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	keyA, keyB := fmt.Sprintf("%064x", 301), fmt.Sprintf("%064x", 302)
	events := []model.Event{reviewEvent(t, base, 300, start.Add(time.Minute), keyA, ""), reviewEvent(t, base, 301, start.Add(25*time.Hour), keyB, "")}
	events[1].ProxyRequestID = events[0].ProxyRequestID
	events[1].Model = "review-model-other"
	events[1].Provider = "review-provider-other"
	events[1].Tokens.Quality = model.TokenQualityEstimated
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	for _, keys := range [][]string{nil, {keyA}, {keyA, keyB}} {
		for _, dimension := range []string{"key", "model", "provider"} {
			query := model.Query{SchemaVersion: 2, Operation: model.OperationDimensions, Start: start, End: start.Add(7 * 24 * time.Hour), TimeZone: "UTC", Dimension: dimension, KeyIDs: keys, PageSize: 20}
			got, err := database.Dimensions(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			query.TimeZone = "Etc/UTC"
			want, err := database.Dimensions(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Rows, want.Rows) {
				t.Fatalf("%s keys=%v mismatch\ngot=%+v\nwant=%+v", dimension, keys, got.Rows, want.Rows)
			}
		}
		query := model.Query{SchemaVersion: 2, Operation: model.OperationTimeseries, Start: start, End: start.Add(7 * 24 * time.Hour), TimeZone: "UTC", BucketWidth: "1w", KeyIDs: keys}
		got, err := database.Timeseries(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		query.TimeZone = "Etc/UTC"
		want, err := database.Timeseries(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Points, want.Points) {
			t.Fatalf("weekly keys=%v mismatch\ngot=%+v\nwant=%+v", keys, got.Points, want.Points)
		}
	}
}

func TestCompactReviewRepriceChunkAndRetentionPreserveCache(t *testing.T) {
	ctx := context.Background()
	database := openMaterialized15mTestStore(t)
	base := loadFixtureEvents(t)[0]
	start := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	key := fmt.Sprintf("%064x", 401)
	events := []model.Event{reviewEvent(t, base, 400, start.Add(time.Minute), key, ""), reviewEvent(t, base, 401, start.Add(2*time.Minute), key, ""), reviewEvent(t, base, 402, start.Add(25*time.Hour), key, "")}
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	rate := model.NanoUSD(1_000_000_000)
	if _, err := database.UpdatePriceBook(ctx, aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "review-price", Model: "review-model", InputPerMillion: &rate, OutputPerMillion: &rate, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "test"}}}); err != nil {
		t.Fatal(err)
	}
	options := RepriceOptions{Range: model.Range{Start: start, End: start.Add(48 * time.Hour), TimeZone: "UTC"}, ChunkSize: 1}
	for {
		result, err := database.Reprice(ctx, options, nil)
		if err != nil {
			t.Fatal(err)
		}
		var compactCost, rawCost int64
		if err := database.db.QueryRowContext(ctx, `SELECT SUM(known_cost_nano) FROM analytics_15m_compact WHERE dimension_kind='overall'`).Scan(&compactCost); err != nil {
			t.Fatal(err)
		}
		if err := database.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(known_cost_nano),0) FROM events`).Scan(&rawCost); err != nil || rawCost != compactCost {
			t.Fatalf("reprice chunk compact cost=%d raw cost=%d err=%v", compactCost, rawCost, err)
		}
		if result.Completed {
			break
		}
		options.ResumeCheckpoint = result.Checkpoint
	}
	if _, err := database.ApplyRetention(ctx, start.Add(24*time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	var retainedCompact, retainedIDs int64
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM analytics_15m_compact WHERE bucket_start_ns<?`, start.Add(24*time.Hour).UnixNano()).Scan(&retainedCompact); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM analytics_15m_request_ids WHERE bucket_start_ns<?`, start.Add(24*time.Hour).UnixNano()).Scan(&retainedIDs); err != nil {
		t.Fatal(err)
	}
	if retainedCompact != 0 || retainedIDs != 0 {
		t.Fatalf("retention left compact=%d identities=%d", retainedCompact, retainedIDs)
	}
	query := model.Query{SchemaVersion: 2, Operation: model.OperationTimeseries, Start: start.Add(24 * time.Hour), End: start.Add(48 * time.Hour), TimeZone: "UTC", BucketWidth: "1h"}
	got, err := database.Timeseries(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	query.TimeZone = "Etc/UTC"
	want, err := database.Timeseries(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Points, want.Points) {
		t.Fatalf("raw suffix after retention differs\ngot=%+v\nwant=%+v", got.Points, want.Points)
	}
}

func TestCompactReviewRollbackImportRebuildsSharedRows(t *testing.T) {
	ctx := context.Background()
	database := openMaterialized15mTestStore(t)
	base := loadFixtureEvents(t)[0]
	start := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	kept := reviewEvent(t, base, 1, start.Add(time.Minute), fmt.Sprintf("%064x", 1), "")
	batchID := "review-import"
	removed := reviewEvent(t, base, 2, start.Add(2*time.Minute), fmt.Sprintf("%064x", 2), batchID)
	if err := database.WriteBatch(ctx, []model.Event{kept}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.WriteImportBatch(ctx, []model.Event{removed}, batchID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.RollbackImport(ctx, batchID); err != nil {
		t.Fatal(err)
	}
	var attempts, input, identities int64
	if err := database.db.QueryRowContext(ctx, `SELECT upstream_attempts,input_tokens FROM analytics_15m_compact
WHERE dimension_kind='overall' AND bucket_start_ns=?`, start.UnixNano()).Scan(&attempts, &input); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM analytics_15m_request_ids WHERE bucket_start_ns=?`, start.UnixNano()).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || input != kept.Tokens.Input || identities != 1 {
		t.Fatalf("rollback left attempts=%d input=%d identities=%d", attempts, input, identities)
	}
}

func TestCompactReviewPurgeKeyRebuildsAllDimensions(t *testing.T) {
	ctx := context.Background()
	database := openMaterialized15mTestStore(t)
	base := loadFixtureEvents(t)[0]
	start := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	keyA := fmt.Sprintf("%064x", 11)
	keyB := fmt.Sprintf("%064x", 12)
	events := []model.Event{reviewEvent(t, base, 3, start.Add(time.Minute), keyA, ""), reviewEvent(t, base, 4, start.Add(2*time.Minute), keyB, "")}
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	if _, err := database.PurgeByKeyID(ctx, keyA); err != nil {
		t.Fatal(err)
	}
	var overallAttempts, overallInput, keyRows, identities int64
	if err := database.db.QueryRowContext(ctx, `SELECT upstream_attempts,input_tokens FROM analytics_15m_compact
WHERE dimension_kind='overall' AND bucket_start_ns=?`, start.UnixNano()).Scan(&overallAttempts, &overallInput); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM analytics_15m_compact WHERE dimension_kind='key' AND dimension_value=?`, keyA).Scan(&keyRows); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM analytics_15m_request_ids WHERE key_id=?`, keyA).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if overallAttempts != 1 || overallInput != events[1].Tokens.Input || keyRows != 0 || identities != 0 {
		t.Fatalf("purge left overall attempts=%d input=%d key rows=%d identities=%d", overallAttempts, overallInput, keyRows, identities)
	}
}

func TestCompactReviewUnalignedAndNonUTCFallbackMatchRaw(t *testing.T) {
	ctx := context.Background()
	database := openMaterialized15mTestStore(t)
	base := loadFixtureEvents(t)[0]
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	events := []model.Event{reviewEvent(t, base, 5, start.Add(time.Minute), fmt.Sprintf("%064x", 21), ""), reviewEvent(t, base, 6, start.Add(16*time.Minute), fmt.Sprintf("%064x", 22), "")}
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	for _, query := range []model.Query{
		{SchemaVersion: model.QuerySchemaVersionV2, Operation: model.OperationTimeseries, Start: start.Add(time.Minute), End: start.Add(time.Hour), TimeZone: "UTC", BucketWidth: "30m"},
		{SchemaVersion: model.QuerySchemaVersionV2, Operation: model.OperationTimeseries, Start: start, End: start.Add(time.Hour), TimeZone: "Asia/Bangkok", BucketWidth: "30m"},
	} {
		got, err := database.Timeseries(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		raw := query
		if query.TimeZone == "UTC" {
			raw.TimeZone = "Etc/UTC"
		}
		want, err := database.Timeseries(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Points, want.Points) {
			t.Fatalf("fallback mismatch for %+v\ngot=%+v\nwant=%+v", query, got.Points, want.Points)
		}
	}
}

func TestCompactReviewConcurrentReadersAndWriters(t *testing.T) {
	ctx := context.Background()
	database := openMaterialized15mTestStore(t)
	base := loadFixtureEvents(t)[0]
	start := time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC)
	queries := model.Query{SchemaVersion: model.QuerySchemaVersionV2, Operation: model.OperationTimeseries, Start: start, End: start.Add(48 * time.Hour), TimeZone: "UTC", BucketWidth: "1h"}
	var group sync.WaitGroup
	errCh := make(chan error, 8)
	for worker := 0; worker < 4; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for index := 0; index < 10; index++ {
				event := reviewEvent(t, base, 100+worker*10+index, start.Add(time.Duration(index)*time.Hour), fmt.Sprintf("%064x", worker+100), "")
				if err := database.WriteBatch(ctx, []model.Event{event}); err != nil {
					errCh <- err
					return
				}
			}
		}(worker)
	}
	for reader := 0; reader < 4; reader++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := 0; index < 10; index++ {
				result, err := database.Timeseries(ctx, queries)
				if err != nil {
					errCh <- err
					return
				}
				for _, point := range result.Points {
					if point.UpstreamAttempts != point.ProxyRequests {
						errCh <- fmt.Errorf("mixed compact snapshot: attempts=%d requests=%d", point.UpstreamAttempts, point.ProxyRequests)
						return
					}
				}
			}
		}()
	}
	group.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}
