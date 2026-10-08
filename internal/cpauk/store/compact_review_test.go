package store

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

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
				if _, err := database.Timeseries(ctx, queries); err != nil {
					errCh <- err
					return
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
