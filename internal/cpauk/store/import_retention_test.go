package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func TestImportedRowsRetainAndRollbackAcrossRollupGrains(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	event := loadFixtureEvents(t)[0]
	if _, err := database.WriteImportBatch(ctx, []model.Event{event}, "batch-retained"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ApplyRetentionPolicy(ctx, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 1); err != nil {
		t.Fatal(err)
	}
	var daily int
	if err := database.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM rollups WHERE grain='daily' AND import_batch_id='batch-retained'").Scan(&daily); err != nil {
		t.Fatal(err)
	}
	if daily == 0 {
		t.Fatal("imported event did not reach daily retention")
	}
	removed, err := database.RollbackImport(ctx, "batch-retained")
	if err != nil {
		t.Fatal(err)
	}
	if removed == 0 {
		t.Fatal("retained import rollback removed no rows")
	}
	query := model.Query{SchemaVersion: 1, Operation: model.OperationSummary,
		Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), TimeZone: "UTC"}
	summary, err := database.Summary(ctx, query)
	if err != nil || summary.UpstreamAttempts != 0 {
		t.Fatalf("summary after retained rollback=%+v err=%v", summary, err)
	}
}
