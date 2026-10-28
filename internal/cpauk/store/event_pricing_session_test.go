package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEventPricingSnapshotSessionAndReprice(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(ctx) }()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	input, output := model.NanoUSD(1_000_000_000), model.NanoUSD(4_000_000_000)
	rule := aggregate.PricingRule{ID: "manual-tier", Model: "model-a", Source: "admin", InputPerMillion: &input, OutputPerMillion: &output, ServiceTierPrices: map[string]aggregate.ServiceTierPrice{"priority": {InputPerMillion: 2_000_000_000, OutputPerMillion: 8_000_000_000, CacheReadMultiplier: "0.1", CacheCreationMultiplier: "1.25"}}}
	if _, err = db.UpdatePriceBook(ctx, aggregate.PriceBook{Rules: []aggregate.PricingRule{rule}}); err != nil {
		t.Fatal(err)
	}
	event := v2Event(strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("a", 64), start.Add(time.Minute), true, nil, 0, 0, 100, 20)
	event.Model = "model-a"
	tier, session := "priority", "header:b2-session"
	event.ServiceTierUsed = &tier
	event.SessionID = &session
	event.Tokens.CacheRead = 10
	event.Tokens.CacheCreation = 20
	// Intake cannot forge a stored pricing snapshot.
	event.PricingBreakdown = &model.PricingBreakdown{}
	if err = db.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	q := v2Query(model.OperationEvents, start, start.Add(time.Hour))
	q.Filters = map[string]json.RawMessage{"session_id": json.RawMessage(`["b2-session"]`)}
	page, err := db.Events(ctx, q)
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("session page=%+v err=%v", page, err)
	}
	recorded := page.Events[0]
	check := func(e model.Event) {
		t.Helper()
		b := e.PricingBreakdown
		if b == nil || e.KnownCost == nil || b.Input.Cost+b.Output.Cost+b.CacheRead.Cost+b.CacheWrite.Cost != *e.KnownCost {
			t.Fatalf("inconsistent pricing: %+v", e)
		}
	}
	check(recorded)
	b := recorded.PricingBreakdown
	if b.Input.RatePerMillion != "2" || b.Output.RatePerMillion != "8" || b.CacheRead.RatePerMillion != "0.2" || b.CacheWrite.RatePerMillion != "2.5" || b.InputTierMultiplier == nil || *b.InputTierMultiplier != "2" || recorded.SessionID == nil || *recorded.SessionID != "b2-session" || recorded.SessionSource == nil || *recorded.SessionSource != "header" {
		t.Fatalf("snapshot=%+v event=%+v", b, recorded)
	}
	q.Filters["session_id"] = json.RawMessage(`["b2-sess"]`)
	page, err = db.Events(ctx, q)
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("filter not exact: %+v %v", page, err)
	}
	// Changing the catalog does not change stored evidence until maintenance reprices it.
	rule.ServiceTierPrices = nil
	if _, err = db.UpdatePriceBook(ctx, aggregate.PriceBook{Rules: []aggregate.PricingRule{rule}}); err != nil {
		t.Fatal(err)
	}
	q.Filters = nil
	page, err = db.Events(ctx, q)
	if err != nil || page.Events[0].PricingBreakdown.Input.RatePerMillion != "2" {
		t.Fatalf("snapshot changed: %+v %v", page, err)
	}
	if _, err = db.Reprice(ctx, RepriceOptions{Range: model.Range{Start: start, End: start.Add(time.Hour), TimeZone: "UTC"}}, nil); err != nil {
		t.Fatal(err)
	}
	page, err = db.Events(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	check(page.Events[0])
	if page.Events[0].PricingBreakdown.Input.RatePerMillion != "1" || *page.Events[0].KnownCost == *recorded.KnownCost {
		t.Fatalf("reprice did not update snapshot: %+v", page)
	}
}

func TestEventSnapshotMigrationKeepsHistoricEvidenceMissing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.db")
	db, err := Open(ctx, Config{Path: path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	e := v2Event(strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("a", 64), start.Add(time.Minute), true, nil, 0, 0, 100, 20)
	if err = db.WriteBatch(ctx, []model.Event{e}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close(ctx)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.ExecContext(ctx, `ALTER TABLE events DROP COLUMN cancellation_source; ALTER TABLE events DROP COLUMN failure_hop; ALTER TABLE events DROP COLUMN outcome; DROP INDEX events_session_id_idx; ALTER TABLE events DROP COLUMN session_id; ALTER TABLE events DROP COLUMN pricing_breakdown; ALTER TABLE events DROP COLUMN routing_trace_json; DELETE FROM schema_migrations WHERE version>=18`)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	db, err = Open(ctx, Config{Path: path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(ctx) }()
	page, err := db.Events(ctx, v2Query(model.OperationEvents, start, start.Add(time.Hour)))
	if err != nil || len(page.Events) != 1 || page.Events[0].SessionID != nil || page.Events[0].PricingBreakdown != nil {
		t.Fatalf("migration invented evidence: %+v %v", page, err)
	}
}

func TestRawSessionFilterAcrossSourcesUsesIndex(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(ctx) }()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i, session := range []string{"header:client-session", "codex:client-session", "claude:client-session", "client-session", "header:client-session-longer"} {
		id := strings.Repeat(string(rune('1'+i)), 32)
		e := v2Event(id, id, strings.Repeat("a", 64), start.Add(time.Duration(i+1)*time.Minute), true, nil, 0, 0, 1, 1)
		e.SessionID = &session
		if err = db.WriteBatch(ctx, []model.Event{e}); err != nil {
			t.Fatal(err)
		}
	}
	q := v2Query(model.OperationEvents, start, start.Add(time.Hour))
	q.Filters = map[string]json.RawMessage{"session_id": json.RawMessage(`["client-session"]`)}
	page, err := db.Events(ctx, q)
	if err != nil || len(page.Events) != 4 {
		t.Fatalf("raw filter page=%+v err=%v", page, err)
	}
	for _, e := range page.Events {
		if e.SessionID == nil || *e.SessionID != "client-session" {
			t.Fatalf("display retained namespace: %+v", e)
		}
	}
	where, args, err := buildWhere(q)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.db.QueryContext(ctx, "EXPLAIN QUERY PLAN SELECT attempt_id FROM events "+where, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	usedIndex := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		usedIndex = usedIndex || strings.Contains(detail, "events_session_id_idx")
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !usedIndex {
		t.Fatal("raw session filter did not use the session index")
	}
	// An event exported with a separate source preserves its namespace on import.
	imported := page.Events[1]
	imported.AttemptID = strings.Repeat("9", 32)
	if err = db.WriteBatch(ctx, []model.Event{imported}); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err = db.db.QueryRowContext(ctx, "SELECT session_id FROM events WHERE attempt_id=?", imported.AttemptID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if imported.SessionSource != nil && stored != *imported.SessionSource+":client-session" {
		t.Fatalf("import lost session namespace: %q", stored)
	}
}

func TestDerivedSessionIsNotAClientSessionFilter(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(ctx) }()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for index, id := range []string{"derived:ctx:v1:digest", "ctx:v1:digest", "lcp:v1:digest", "header:actual-client-session", "header:derived:ctx:v1:client-literal"} {
		e := v2Event(fmt.Sprintf("%032x", index+1), strings.Repeat("2", 32), strings.Repeat("a", 64), start.Add(time.Minute), true, nil, 0, 0, 1, 1)
		e.SessionID = &id
		if err = db.WriteBatch(ctx, []model.Event{e}); err != nil {
			t.Fatal(err)
		}
	}
	q := v2Query(model.OperationEvents, start, start.Add(time.Hour))
	for _, test := range []struct {
		id    string
		count int
	}{
		{"derived:ctx:v1:digest", 0}, {"ctx:v1:digest", 0}, {"lcp:v1:digest", 0},
		{"actual-client-session", 1}, {"derived:ctx:v1:client-literal", 1},
	} {
		encoded, _ := json.Marshal([]string{test.id})
		q.Filters = map[string]json.RawMessage{"session_id": encoded}
		page, err := db.Events(ctx, q)
		if err != nil || len(page.Events) != test.count {
			t.Fatalf("filter %q: %+v %v", test.id, page, err)
		}
	}
	q.Filters = nil
	page, err := db.Events(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Events {
		if event.SessionID != nil && *event.SessionID == "derived:ctx:v1:digest" && (event.SessionSource == nil || *event.SessionSource != "derived") {
			t.Fatal("internal identity not marked derived")
		}
	}
}
