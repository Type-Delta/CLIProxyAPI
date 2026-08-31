package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func TestCreateWriteQueryBackupRestoreAndRetention(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	book := fixturePriceBook()
	codec, err := model.NewCursorCodec(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	database, err := Open(ctx, Config{
		Path: filepath.Join(directory, "analytics.db"), MaxStorageBytes: 64 << 20,
		PriceBook: book, CursorCodec: codec,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	if database.SchemaVersion() != 1 || database.IdentityEpoch() == "" {
		t.Fatalf("schema=%d epoch=%q", database.SchemaVersion(), database.IdentityEpoch())
	}
	events := loadFixtureEvents(t)
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	query := model.Query{SchemaVersion: 1, Operation: model.OperationSummary,
		Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), TimeZone: "UTC"}
	if err := query.Validate(); err != nil {
		t.Fatal(err)
	}
	summary, err := database.Summary(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProxyRequests != 2 || summary.UpstreamAttempts != 3 || summary.Tokens.Total != 400 || summary.KnownCost.String() != "0.002157525" || summary.UnpricedTokens != 200 {
		t.Fatalf("summary did not reconcile: %+v", summary)
	}
	query.KeyIDs = []string{events[0].KeyID, events[1].KeyID}
	summary, err = database.Summary(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProxyRequests != 1 || summary.UpstreamAttempts != 2 || summary.Tokens.Total != 200 || summary.KnownCost.String() != "0.002157525" {
		t.Fatalf("multi-key summary did not reconcile: %+v", summary)
	}

	timeseriesQuery := model.Query{SchemaVersion: 1, Operation: model.OperationTimeseries,
		Start: query.Start, End: query.End, TimeZone: "UTC", KeyIDs: append([]string(nil), query.KeyIDs...), BucketWidth: "1h"}
	if err := timeseriesQuery.Validate(); err != nil {
		t.Fatal(err)
	}
	timeseries, err := database.Timeseries(ctx, timeseriesQuery)
	if err != nil || len(timeseries.Points) != 1 || timeseries.Points[0].ProxyRequests != 1 || timeseries.Points[0].UpstreamAttempts != 2 || timeseries.Points[0].Tokens.Total != 200 {
		t.Fatalf("timeseries=%+v err=%v", timeseries, err)
	}

	dimensionQuery := model.Query{SchemaVersion: 1, Operation: model.OperationDimensions,
		Start: query.Start, End: query.End, TimeZone: "UTC", Dimension: "provider", PageSize: 1}
	if err := dimensionQuery.Validate(); err != nil {
		t.Fatal(err)
	}
	dimensions, err := database.Dimensions(ctx, dimensionQuery)
	if err != nil || len(dimensions.Rows) != 1 || dimensions.Rows[0].Tokens.Total != 200 || dimensions.Meta.NextCursor == "" {
		t.Fatalf("dimensions=%+v err=%v", dimensions, err)
	}
	dimensionQuery.Cursor = dimensions.Meta.NextCursor
	if err := dimensionQuery.ValidateCursor(codec, model.CursorTransportBody); err != nil {
		t.Fatal(err)
	}
	dimensions, err = database.Dimensions(ctx, dimensionQuery)
	if err != nil || len(dimensions.Rows) != 1 || dimensions.Rows[0].Tokens.Total != 200 || dimensions.Meta.NextCursor != "" {
		t.Fatalf("dimensions page 2=%+v err=%v", dimensions, err)
	}

	eventsQuery := model.Query{SchemaVersion: 1, Operation: model.OperationEvents,
		Start: query.Start, End: query.End, TimeZone: "UTC", PageSize: 2}
	if err := eventsQuery.Validate(); err != nil {
		t.Fatal(err)
	}
	eventPage, err := database.Events(ctx, eventsQuery)
	if err != nil || len(eventPage.Events) != 2 || eventPage.Meta.NextCursor == "" {
		t.Fatalf("events page 1=%+v err=%v", eventPage, err)
	}
	eventsQuery.Cursor = eventPage.Meta.NextCursor
	if err := eventsQuery.ValidateCursor(codec, model.CursorTransportBody); err != nil {
		t.Fatal(err)
	}
	eventPage, err = database.Events(ctx, eventsQuery)
	if err != nil || len(eventPage.Events) != 1 || eventPage.Meta.NextCursor != "" {
		t.Fatalf("events page 2=%+v err=%v", eventPage, err)
	}
	eventsQuery.Cursor = ""
	event, found, err := database.EventByAttemptID(ctx, events[1].AttemptID, eventsQuery)
	if err != nil || !found || event.AttemptID != events[1].AttemptID {
		t.Fatalf("indexed event=%+v found=%t err=%v", event, found, err)
	}
	eventsQuery.Filters = map[string]json.RawMessage{"provider": json.RawMessage(`["not-the-provider"]`)}
	if _, found, err = database.EventByAttemptID(ctx, events[1].AttemptID, eventsQuery); err != nil || found {
		t.Fatalf("filtered indexed event found=%t err=%v", found, err)
	}

	leaderboardQuery := model.Query{SchemaVersion: 1, Operation: model.OperationLeaderboard,
		Start: query.Start, End: query.End, TimeZone: "UTC", PageSize: 2, SortBy: model.LeaderboardSortTokens}
	if err := leaderboardQuery.Validate(); err != nil {
		t.Fatal(err)
	}
	leaderboard, err := database.Leaderboard(ctx, leaderboardQuery)
	if err != nil || len(leaderboard.Rows) != 2 || leaderboard.Rows[0].Tokens.Total != 200 || leaderboard.Rows[0].Rank != 1 || leaderboard.Meta.NextCursor == "" {
		t.Fatalf("leaderboard page 1=%+v err=%v", leaderboard, err)
	}
	leaderboardQuery.Cursor = leaderboard.Meta.NextCursor
	if err := leaderboardQuery.ValidateCursor(codec, model.CursorTransportBody); err != nil {
		t.Fatal(err)
	}
	leaderboard, err = database.Leaderboard(ctx, leaderboardQuery)
	if err != nil || len(leaderboard.Rows) != 1 || leaderboard.Rows[0].Rank != 3 || leaderboard.Meta.NextCursor != "" {
		t.Fatalf("leaderboard page 2=%+v err=%v", leaderboard, err)
	}

	backupPath := filepath.Join(directory, "backup.db")
	manifest, err := database.Backup(ctx, backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackup(ctx, backupPath, backupPath+".manifest.json"); err != nil {
		t.Fatal(err)
	}
	manifestData, err := os.ReadFile(backupPath + ".manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupPath+".manifest.tampered.json", append(manifestData, []byte(`{"extra":true}`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackup(ctx, backupPath, backupPath+".manifest.tampered.json"); err == nil {
		t.Fatal("manifest trailing data passed verification")
	}
	if manifest.IdentityEpoch != database.IdentityEpoch() {
		t.Fatal("backup identity epoch changed")
	}
	if _, err := database.PurgeByKeyID(ctx, events[2].KeyID); err != nil {
		t.Fatal(err)
	}
	if err := database.Restore(ctx, backupPath, backupPath+".manifest.json"); err != nil {
		t.Fatal(err)
	}
	query.KeyIDs = nil
	summary, err = database.Summary(ctx, query)
	if err != nil || summary.UpstreamAttempts != 3 {
		t.Fatalf("restore summary=%+v err=%v", summary, err)
	}

	retention, err := database.ApplyRetention(ctx, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 2)
	if err != nil {
		t.Fatal(err)
	}
	if retention.DeletedRows != 3 || retention.RolledUpRows == 0 {
		t.Fatalf("retention=%+v", retention)
	}
	var raw, rolled int64
	if err := database.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(upstream_attempts),0) FROM rollups WHERE grain='hourly'").Scan(&rolled); err != nil {
		t.Fatal(err)
	}
	if raw != 0 || rolled != 3 {
		t.Fatalf("raw=%d rolled=%d", raw, rolled)
	}
	summary, err = database.Summary(ctx, query)
	if err != nil || summary.ProxyRequests != 2 || summary.UpstreamAttempts != 3 || summary.Tokens.Total != 400 || summary.KnownCost.String() != "0.002157525" || summary.UnpricedTokens != 200 {
		t.Fatalf("retained summary=%+v err=%v", summary, err)
	}
}

func TestIdentityLossAndMigrationChecksumArePermanent(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	config := Config{Path: filepath.Join(directory, "analytics.db"), MaxStorageBytes: 64 << 20}
	database, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, "UPDATE schema_migrations SET checksum='wrong' WHERE version=1"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, config); !errors.Is(err, ErrMigrationChecksum) {
		t.Fatalf("checksum error=%v", err)
	}
	if err := os.Rename(filepath.Join(directory, "identity.key"), filepath.Join(directory, "identity.saved")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, config); !errors.Is(err, ErrIdentityKeyMissing) {
		t.Fatalf("missing identity error=%v", err)
	}
}

func TestCorruptDatabaseIsNotReplaced(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "analytics.db")
	identityPath := filepath.Join(directory, "identity.key")
	if err := os.WriteFile(databasePath, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identityPath, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), Config{Path: databasePath, MaxStorageBytes: 64 << 20})
	if err == nil {
		t.Fatal("corrupt database opened")
	}
	data, readErr := os.ReadFile(databasePath)
	if readErr != nil || string(data) != "not a sqlite database" {
		t.Fatalf("corrupt source was changed: data=%q err=%v", data, readErr)
	}
}

func TestQuotaRejectsBatchBeforeWrite(t *testing.T) {
	directory := t.TempDir()
	database, err := Open(context.Background(), Config{Path: filepath.Join(directory, "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	database.config.MaxStorageBytes = database.databaseSize() + 1
	err = database.WriteBatch(context.Background(), loadFixtureEvents(t)[:1])
	if !errors.Is(err, ErrStorageQuota) {
		t.Fatalf("quota error=%v", err)
	}
}

func TestBusyWriterHonorsContext(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	database, err := Open(ctx, Config{Path: filepath.Join(directory, "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	connection, err := database.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if _, err := connection.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = connection.ExecContext(context.Background(), "ROLLBACK") }()
	writeContext, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	err = database.WriteBatch(writeContext, loadFixtureEvents(t)[:1])
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy write error=%v", err)
	}
}

func TestPermanentErrorClassification(t *testing.T) {
	for _, testCase := range []struct {
		err      error
		category string
	}{
		{ErrIdentityKeyMissing, "identity_key"}, {ErrMigrationChecksum, "migration"},
		{ErrCorruptDatabase, "storage_corrupt"}, {ErrStorageQuota, "storage_quota"},
	} {
		classified, ok := testCase.err.(interface {
			Permanent() bool
			Category() string
		})
		if !ok || !classified.Permanent() || classified.Category() != testCase.category {
			t.Fatalf("classification for %v is wrong", testCase.err)
		}
	}
}

func loadFixtureEvents(t *testing.T) []model.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "upstream-v1.15.0", "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Events []model.Event `json:"events"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture.Events
}

func fixturePriceBook() aggregate.PriceBook {
	input := model.NanoUSD(10_035_000_000)
	output := model.NanoUSD(15_052_500_000)
	return aggregate.PriceBook{Rules: []aggregate.PricingRule{{
		ID: "price-04e81c", Model: "model-f93b", InputPerMillion: &input, OutputPerMillion: &output,
		CacheReadMultiplier: "0", CacheCreationMultiplier: "0", Source: "fixture-9a70e2",
	}}}
}
