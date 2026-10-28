package chstore

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestSessionCaptureEvidenceDistinguishesMissingAndHistoricalIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := openR3Store(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := db.setMetadata(ctx, "session_capture_since_ns", strconv.FormatInt(start.Add(30*time.Minute).UnixNano(), 10)); err != nil {
		t.Fatal(err)
	}
	makeEvent := func(id string, at time.Time) model.Event {
		return r3V2Event(strings.Repeat(id, 32), strings.Repeat(id, 32), strings.Repeat("a", 64), at, true, nil, 0, 0, 0, 0)
	}
	old := makeEvent("1", start.Add(time.Minute))
	postMigration := makeEvent("2", start.Add(40*time.Minute))
	explicitOutcome := makeEvent("3", start.Add(2*time.Minute))
	explicitOutcome.Outcome = "succeeded"
	if err := db.WriteBatch(ctx, []model.Event{old, postMigration, explicitOutcome}); err != nil {
		t.Fatal(err)
	}
	imported := makeEvent("4", start.Add(45*time.Minute))
	if _, err := db.WriteImportBatch(ctx, []model.Event{imported}, "historic"); err != nil {
		t.Fatal(err)
	}
	query := r3V2Query(model.OperationEvents, start, start.Add(time.Hour))
	page, err := db.Events(ctx, query)
	if err != nil || len(page.Events) != 4 {
		t.Fatalf("events=%+v err=%v", page, err)
	}
	for _, event := range page.Events {
		expected := event.AttemptID == postMigration.AttemptID || event.AttemptID == explicitOutcome.AttemptID
		if event.SessionCaptured != expected || event.SessionID != nil {
			t.Fatalf("capture evidence: %+v expected=%v", event, expected)
		}
		detail, found, err := db.EventByAttemptID(ctx, event.AttemptID, query)
		if err != nil || !found || detail.SessionCaptured != expected {
			t.Fatalf("detail capture=%v found=%v err=%v", detail.SessionCaptured, found, err)
		}
	}
}
