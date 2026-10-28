package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestOutcomeStorageLegacyFilterAndAggregateCompatibility(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	makeEvent := func(id string, success bool) model.Event {
		return v2Event(strings.Repeat(id, 32), strings.Repeat(id, 32), strings.Repeat("a", 64), start.Add(time.Minute), success, nil, 0, 0, 10, 20)
	}
	client, hop, unknown := "client", "provider_to_cpa", "unknown"
	canceled := makeEvent("1", true)
	canceled.Outcome = "canceled"
	canceled.CancellationSource = &client
	failed := makeEvent("2", false)
	failed.Outcome = "failed"
	failed.FailureHop = &hop
	status := 200
	failed.UpstreamStatusCode = &status
	body := `{"error":{"code":"server_is_overloaded"}}`
	failed.UpstreamErrorBody = &body
	legacy := makeEvent("3", false)
	cancelBody := "context canceled"
	legacy.UpstreamErrorBody = &cancelBody
	success := makeEvent("4", true)
	success.Outcome = "succeeded"
	if err := db.WriteBatch(ctx, []model.Event{canceled, failed, legacy, success}); err != nil {
		t.Fatal(err)
	}
	q := v2Query(model.OperationEvents, start, start.Add(time.Hour))
	q.Filters = map[string]json.RawMessage{"outcome": json.RawMessage(`"canceled"`)}
	page, err := db.Events(ctx, q)
	if err != nil || len(page.Events) != 2 || page.TotalCount != 2 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	for _, e := range page.Events {
		if e.Outcome != "canceled" || !e.Succeeded || e.FailureHop != nil || e.Completed() {
			t.Fatalf("canceled=%+v", e)
		}
		if e.AttemptID == legacy.AttemptID && (e.CancellationSource == nil || *e.CancellationSource != unknown) {
			t.Fatalf("legacy=%+v", e)
		}
	}
	q.Filters = nil
	summaryQuery := v2Query(model.OperationSummary, start, start.Add(time.Hour))
	summaryQuery.PageSize = 0
	summary, err := db.Summary(ctx, summaryQuery)
	if err != nil || summary.Succeeded != 2 || summary.ProcessingTime.SampleCount != 3 {
		t.Fatalf("aggregate/latency: %+v err=%v", summary, err)
	}
	got, found, err := db.EventByAttemptID(ctx, failed.AttemptID, q)
	if err != nil || !found || got.FailureHop == nil || *got.FailureHop != hop || *got.UpstreamStatusCode != 200 {
		t.Fatalf("failure=%+v found=%v err=%v", got, found, err)
	}
	var succeeded int
	if err := db.db.QueryRowContext(ctx, "SELECT SUM(succeeded) FROM events").Scan(&succeeded); err != nil || succeeded != 2 {
		t.Fatalf("stored aggregate=%d err=%v", succeeded, err)
	}
	var legacyOutcome any
	if err := db.db.QueryRowContext(ctx, "SELECT outcome FROM events WHERE attempt_id=?", legacy.AttemptID).Scan(&legacyOutcome); err != nil || legacyOutcome != nil {
		t.Fatal("read classification rewrote legacy row")
	}
	q.Filters = map[string]json.RawMessage{"outcome": json.RawMessage(`"canceled"`), "success": json.RawMessage(`false`)}
	if _, err := db.Events(ctx, q); err == nil {
		t.Fatal("contradictory filters accepted")
	}
}
