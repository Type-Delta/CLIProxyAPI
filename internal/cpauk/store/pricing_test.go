package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestServiceTierPricingMigrationPreservesExistingPrices(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "analytics.db")
	fetcher := &testPricingFetcher{catalog: testPricingCatalog(1_000_000_000, 2_000_000_000)}
	database := openDiscoveryStore(t, path, fetcher, &now)
	if _, err := database.RefreshPricing(ctx); err != nil {
		t.Fatal(err)
	}
	manualInput, manualOutput := model.NanoUSD(3_000_000_000), model.NanoUSD(4_000_000_000)
	if _, err := database.UpdatePriceBook(ctx, aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "manual", Model: "manual-model", InputPerMillion: &manualInput, OutputPerMillion: &manualOutput, Source: "admin"}}}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.ExecContext(ctx, `ALTER TABLE events DROP COLUMN cancellation_source; ALTER TABLE events DROP COLUMN failure_hop; ALTER TABLE events DROP COLUMN outcome; DROP INDEX IF EXISTS events_session_id_idx; ALTER TABLE events DROP COLUMN session_id; ALTER TABLE events DROP COLUMN pricing_breakdown; ALTER TABLE events DROP COLUMN provider_credits; ALTER TABLE events DROP COLUMN price_usd_per_credit_nano; ALTER TABLE pricing_manual_rules DROP COLUMN service_tier_prices;
ALTER TABLE pricing_catalog_rules DROP COLUMN service_tier_prices;
ALTER TABLE events DROP COLUMN routing_trace_json; DELETE FROM schema_migrations WHERE version>=15;`)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	database = openDiscoveryStore(t, path, fetcher, &now)
	if database.SchemaVersion() != latestSchemaVersion(t) {
		t.Fatalf("schema=%d", database.SchemaVersion())
	}
	snapshot, err := database.PricingSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Catalog) != 1 || len(snapshot.Overrides) != 1 || *snapshot.Catalog[0].InputPerMillion != 1_000_000_000 || *snapshot.Overrides[0].InputPerMillion != manualInput || len(snapshot.Catalog[0].ServiceTierPrices) != 0 || !snapshot.Provenance.CatalogExpiresAt.IsZero() {
		t.Fatalf("migrated pricing=%+v", snapshot)
	}
	if _, err := os.Stat(path + ".pre-migration-v15"); err != nil {
		t.Fatal(err)
	}
	if fetcher.count() != 1 {
		t.Fatal("migration must not trigger an eager catalog fetch")
	}
}

func TestDurablePricingCatalogUpdatesWritesAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.db")
	database, err := Open(ctx, Config{Path: path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	events := loadFixtureEvents(t)
	first := events[0]
	if err := database.WriteBatch(ctx, []model.Event{first}); err != nil {
		t.Fatal(err)
	}
	input, output := model.NanoUSD(1_000_000_000), model.NanoUSD(2_000_000_000)
	originalInput := input
	rules := []aggregate.PricingRule{{ID: "catalog-rule-v1", Model: first.Model, InputPerMillion: &input,
		OutputPerMillion: &output, CacheReadMultiplier: "0.1", CacheCreationMultiplier: "1.25", Source: "catalog-fixture"}}
	provenance := PricingProvenance{Source: "catalog-fixture", SourceDigest: strings.Repeat("a", 64),
		SyncedAt: time.Date(2026, 8, 31, 6, 0, 0, 0, time.UTC)}
	if err := database.ReplacePricingRules(ctx, rules, provenance); err != nil {
		t.Fatal(err)
	}
	// Mutation by the caller after replacement must not alter the active catalog.
	*rules[0].InputPerMillion = 99
	second := first
	second.AttemptID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	second.ProxyRequestID = second.AttemptID
	second.RequestedAt = second.RequestedAt.Add(time.Second)
	if err := database.WriteBatch(ctx, []model.Event{second}); err != nil {
		t.Fatal(err)
	}
	var firstCost, secondCost any
	if err := database.db.QueryRowContext(ctx, "SELECT known_cost_nano FROM events WHERE attempt_id=?", first.AttemptID).Scan(&firstCost); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, "SELECT known_cost_nano FROM events WHERE attempt_id=?", second.AttemptID).Scan(&secondCost); err != nil {
		t.Fatal(err)
	}
	if firstCost != nil || secondCost == nil {
		t.Fatalf("cost before=%v after=%v", firstCost, secondCost)
	}
	snapshot, err := database.PricingSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Rules) != 1 || *snapshot.Rules[0].InputPerMillion != originalInput || snapshot.Provenance != provenance {
		t.Fatalf("pricing snapshot = %+v", snapshot)
	}
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}

	database, err = Open(ctx, Config{Path: path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	third := first
	third.AttemptID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	third.ProxyRequestID = third.AttemptID
	third.RequestedAt = third.RequestedAt.Add(2 * time.Second)
	if err := database.WriteBatch(ctx, []model.Event{third}); err != nil {
		t.Fatal(err)
	}
	var thirdCost any
	if err := database.db.QueryRowContext(ctx, "SELECT known_cost_nano FROM events WHERE attempt_id=?", third.AttemptID).Scan(&thirdCost); err != nil {
		t.Fatal(err)
	}
	if thirdCost == nil || thirdCost != secondCost {
		t.Fatalf("cost after restart=%v want=%v", thirdCost, secondCost)
	}
}

func TestPricingCatalogValidationIsBoundedAndTransactional(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	input, output := model.NanoUSD(1), model.NanoUSD(2)
	provenance := PricingProvenance{Source: "fixture", SourceDigest: strings.Repeat("c", 64), SyncedAt: time.Now().UTC()}
	valid := []aggregate.PricingRule{{ID: "valid", Model: "model", InputPerMillion: &input,
		OutputPerMillion: &output, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "fixture"}}
	if err := database.ReplacePricingRules(ctx, valid, provenance); err != nil {
		t.Fatal(err)
	}
	invalid := append([]aggregate.PricingRule(nil), valid...)
	invalid[0].OutputPerMillion = nil
	if err := database.ReplacePricingRules(ctx, invalid, provenance); err == nil {
		t.Fatal("invalid catalog was accepted")
	}
	tooMany := make([]aggregate.PricingRule, MaxPricingRules+1)
	if err := database.ReplacePricingRules(ctx, tooMany, provenance); err == nil {
		t.Fatal("oversized catalog was accepted")
	}
	snapshot, err := database.PricingSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Rules) != 1 || snapshot.Rules[0].ID != "valid" {
		t.Fatalf("failed update changed catalog: %+v", snapshot)
	}
}
