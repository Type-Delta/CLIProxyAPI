package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestServiceTierPricingIngestRestartAndReprice(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	start := now
	path := filepath.Join(t.TempDir(), "analytics.db")
	fetcher := &testPricingFetcher{catalog: testPricingCatalog(1_000_000_000, 2_000_000_000)}
	fetcher.catalog.Rules[0].CacheReadMultiplier = "0.1"
	fetcher.catalog.Rules[0].CacheCreationMultiplier = "1.25"
	database := openDiscoveryStore(t, path, fetcher, &now)
	if _, err := database.RefreshPricing(ctx); err != nil {
		t.Fatal(err)
	}
	priority, standard, flex := "priority", "default", "flex"
	events := make([]model.Event, 4)
	for index := range events {
		event := v2Event(fmt.Sprintf("%032x", index+1), fmt.Sprintf("%032x", index+20), strings.Repeat("a", 64), start.Add(time.Duration(index)*time.Minute), true, nil, 0, 40, 10, 20)
		event.Provider, event.Model = "codex", "gpt-5"
		event.Tokens.CacheCreation = 12
		event.ServiceTierRequested, event.ServiceTierUsed = &priority, &priority
		events[index] = event
	}
	events[2].ServiceTierUsed, events[3].ServiceTierUsed = &standard, &flex
	if err := database.WriteBatch(ctx, events[:1]); err != nil {
		t.Fatal(err)
	}
	fetcher.catalog.Rules[0].ServiceTierPrices = map[string]aggregate.ServiceTierPrice{
		"priority": {InputPerMillion: 2_500_000_000, OutputPerMillion: 5_000_000_000, CacheReadMultiplier: "0.1", CacheCreationMultiplier: "1.25"},
		"flex":     {InputPerMillion: 500_000_000, OutputPerMillion: 1_000_000_000, CacheReadMultiplier: "0.1", CacheCreationMultiplier: "1.25"},
	}
	now = now.Add(PricingCatalogTTL)
	if _, err := database.RefreshPricing(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}
	database = openDiscoveryStore(t, path, fetcher, &now)
	snapshot, err := database.PricingSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Catalog[0].ServiceTierPrices[priority].InputPerMillion != 2_500_000_000 {
		t.Fatalf("lost tier rates on restart: %+v", snapshot)
	}
	// Caller-owned snapshots must not mutate the active price book.
	snapshot.Catalog[0].ServiceTierPrices[priority] = aggregate.ServiceTierPrice{}
	if err := database.WriteBatch(ctx, events[1:]); err != nil {
		t.Fatal(err)
	}
	for index, want := range []int64{267_000, 667_500, 267_000, 133_500} {
		var cost int64
		if err := database.db.QueryRowContext(ctx, "SELECT known_cost_nano FROM events WHERE attempt_id=?", events[index].AttemptID).Scan(&cost); err != nil {
			t.Fatal(err)
		}
		if cost != want {
			t.Fatalf("event %d cost=%d want %d", index, cost, want)
		}
	}
	selected := model.Range{Start: start, End: start.Add(time.Hour), TimeZone: "UTC"}
	result, err := database.Reprice(ctx, RepriceOptions{Range: selected}, nil)
	if err != nil || result.Updated != 4 || !result.Completed {
		t.Fatalf("reprice=%+v err=%v", result, err)
	}
	var total int64
	if err := database.db.QueryRowContext(ctx, "SELECT SUM(known_cost_nano) FROM events").Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1_735_500 {
		t.Fatalf("repriced total=%d", total)
	}
	components, err := database.analysisCosts(ctx, v2Query(model.OperationAnalysis, selected.Start, selected.End))
	if err != nil {
		t.Fatal(err)
	}
	if components.UncachedInputUSD != "0.000312" || components.CacheReadUSD != "0.000026" || components.CacheCreationUSD != "0.0000975" || components.OutputUSD != "0.0013" {
		t.Fatalf("mixed-tier analysis costs=%+v", components)
	}
	if len(components.Models) != 1 || components.Models[0].TotalUSD != "0.0017355" {
		t.Fatalf("model cost components=%+v", components.Models)
	}
}

func TestPricingDiscoveryUpdatesSubsequentIngestPrices(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	fetcher := &testPricingFetcher{catalog: testPricingCatalog(1_000_000_000, 2_000_000_000)}
	database := openDiscoveryStore(t, filepath.Join(t.TempDir(), "analytics.db"), fetcher, &now)
	for index, want := range []int64{300_000, 600_000} {
		if index > 0 {
			now = now.Add(PricingCatalogTTL)
			fetcher.mu.Lock()
			fetcher.catalog = testPricingCatalog(2_000_000_000, 4_000_000_000)
			fetcher.mu.Unlock()
		}
		if _, err := database.RefreshPricing(ctx); err != nil {
			t.Fatal(err)
		}
		event := v2Event(fmt.Sprintf("%032x", index+1), fmt.Sprintf("%032x", index+10), strings.Repeat("a", 64), now, true, nil, 0, 0, 10, 20)
		event.Provider, event.Model = "codex", "gpt-5"
		if err := database.WriteBatch(ctx, []model.Event{event}); err != nil {
			t.Fatal(err)
		}
		var cost sql.NullInt64
		var unpriced int64
		if err := database.db.QueryRowContext(ctx, "SELECT known_cost_nano, unpriced_tokens FROM events WHERE attempt_id=?", event.AttemptID).Scan(&cost, &unpriced); err != nil {
			t.Fatal(err)
		}
		if !cost.Valid || cost.Int64 != want || unpriced != 0 {
			t.Errorf("catalog version %d stored cost=%+v unpriced=%d, want cost=%d unpriced=0", index+1, cost, unpriced, want)
		}
	}
}
