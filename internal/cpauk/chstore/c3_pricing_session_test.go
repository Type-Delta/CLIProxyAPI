package chstore

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestEventPricingSnapshotSessionAndReprice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	var err error
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	input, output := model.NanoUSD(1_000_000_000), model.NanoUSD(4_000_000_000)
	rule := aggregate.PricingRule{ID: "manual-tier", Model: "model-a", Source: "admin", InputPerMillion: &input, OutputPerMillion: &output, ServiceTierPrices: map[string]aggregate.ServiceTierPrice{"priority": {InputPerMillion: 2_000_000_000, OutputPerMillion: 8_000_000_000, CacheReadMultiplier: "0.1", CacheCreationMultiplier: "1.25"}}}
	if _, err = db.UpdatePriceBook(ctx, aggregate.PriceBook{Rules: []aggregate.PricingRule{rule}}); err != nil {
		t.Fatal(err)
	}
	event := c3V2Event(strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("a", 64), start.Add(time.Minute), true, nil, 0, 0, 100, 20)
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
	q := c3V2Query(model.OperationEvents, start, start.Add(time.Hour))
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
