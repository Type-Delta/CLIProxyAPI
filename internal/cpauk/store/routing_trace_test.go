package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestRoutingTraceDetailRoundTripAndMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.db")
	database, err := Open(ctx, Config{Path: path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	event := throughputStoreEvent("1", strings.Repeat("5", 64), time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC), 200, 500, 1000, 480)
	event.RoutingTrace = json.RawMessage(`{"version":1,"mode":"local","retry_round":0,"selection":0,"decided_by":"strategy","candidates":[{"id":"` + strings.Repeat("a", 64) + `","provider":"openai"}],"pick":0}`)
	if err := database.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	query := v2Query(model.OperationEvents, event.RequestedAt.Add(-time.Hour), event.RequestedAt.Add(time.Hour))
	page, err := database.Events(ctx, query)
	if err != nil || len(page.Events) != 1 || len(page.Events[0].RoutingTrace) != 0 {
		t.Fatalf("list returned trace: page=%+v err=%v", page, err)
	}
	exportPage, err := database.EventsForExport(ctx, query)
	if err != nil || len(exportPage.Events) != 1 || string(exportPage.Events[0].RoutingTrace) != string(event.RoutingTrace) {
		t.Fatalf("export projection trace=%s err=%v", exportPage.Events[0].RoutingTrace, err)
	}
	detail, found, err := database.EventByAttemptID(ctx, event.AttemptID, query)
	if err != nil || !found || string(detail.RoutingTrace) != string(event.RoutingTrace) {
		t.Fatalf("detail trace=%s found=%t err=%v", detail.RoutingTrace, found, err)
	}
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `ALTER TABLE events DROP COLUMN routing_trace_json; DELETE FROM schema_migrations WHERE version=20`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, Config{Path: path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(ctx) }()
	if database.SchemaVersion() != 20 {
		t.Fatalf("schema version = %d, want 20", database.SchemaVersion())
	}
	legacy, found, err := database.EventByAttemptID(ctx, event.AttemptID, query)
	if err != nil || !found || len(legacy.RoutingTrace) != 0 {
		t.Fatalf("event after migration=%+v found=%t err=%v", legacy, found, err)
	}
}
