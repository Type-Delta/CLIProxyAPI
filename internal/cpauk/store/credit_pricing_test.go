package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestCreditPricingPersistsRefreshRepriceAndAnalysis(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	start := now
	path := filepath.Join(t.TempDir(), "analytics.db")
	fetcher := &testPricingFetcher{catalog: testPricingCatalog(1_000_000_000, 2_000_000_000)}
	db := openDiscoveryStore(t, path, fetcher, &now)
	rate := model.NanoUSD(30_000_000)
	if _, err := db.UpdatePriceBook(ctx, aggregate.PriceBook{KiroUSDPerCredit: &rate}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RefreshPricing(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(ctx); err != nil {
		t.Fatal(err)
	}
	db = openDiscoveryStore(t, path, fetcher, &now)
	book, err := db.PriceBook(ctx)
	if err != nil || book.EffectiveKiroRate() != rate {
		t.Fatalf("restart rate: %+v %v", book, err)
	}
	*book.KiroUSDPerCredit = 1
	if _, err := db.UpdatePriceBook(ctx, aggregate.PriceBook{}); err != nil {
		t.Fatal(err)
	}
	book, err = db.PriceBook(ctx)
	if err != nil || book.EffectiveKiroRate() != rate {
		t.Fatalf("legacy update: %+v %v", book, err)
	}
	credits := "1.25"
	zero := "0"
	bogus := model.NanoUSD(999)
	first := v2Event(strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("a", 64), start.Add(time.Minute), true, nil, 0, 0, 10, 5)
	first.Provider = "kiro"
	first.ProviderCredits = &credits
	first.PriceUSDPerCredit = &bogus
	second := v2Event(strings.Repeat("3", 32), strings.Repeat("4", 32), strings.Repeat("a", 64), start.Add(2*time.Minute), true, nil, 0, 0, 10, 5)
	second.Provider = "kiro"
	second.ProviderCredits = &zero
	if err := db.WriteBatch(ctx, []model.Event{first, second}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Events(ctx, v2Query(model.OperationEvents, start, start.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Events) != 2 || rows.Events[1].PricingBreakdown != nil || rows.Events[1].KnownCost == nil || *rows.Events[1].KnownCost != 37_500_000 || rows.Events[1].PriceUSDPerCredit == nil || *rows.Events[1].PriceUSDPerCredit != rate || rows.Events[0].KnownCost == nil || *rows.Events[0].KnownCost != 0 {
		t.Fatalf("stored events: %+v", rows.Events)
	}
	analysis, err := db.Analysis(ctx, v2Query(model.OperationAnalysis, start, start.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if analysis.CostComponents.CreditBasedUSD != "0.0375" {
		t.Fatalf("components: %+v", analysis.CostComponents)
	}
	newRate := model.NanoUSD(0)
	if _, err := db.UpdatePriceBook(ctx, aggregate.PriceBook{KiroUSDPerCredit: &newRate}); err != nil {
		t.Fatal(err)
	}
	before, err := db.Analysis(ctx, v2Query(model.OperationAnalysis, start, start.Add(time.Hour)))
	if err != nil || before.CostComponents.CreditBasedUSD != "0.0375" {
		t.Fatalf("stored price before reprice: %+v %v", before.CostComponents, err)
	}
	result, err := db.Reprice(ctx, RepriceOptions{Range: model.Range{Start: start, End: start.Add(time.Hour), TimeZone: "UTC"}}, nil)
	if err != nil || result.Updated != 2 {
		t.Fatalf("reprice: %+v %v", result, err)
	}
	rows, err = db.Events(ctx, v2Query(model.OperationEvents, start, start.Add(time.Hour)))
	if err != nil || rows.Events[1].PricingBreakdown != nil || rows.Events[1].KnownCost == nil || *rows.Events[1].KnownCost != 0 || rows.Events[1].PriceUSDPerCredit == nil || *rows.Events[1].PriceUSDPerCredit != 0 {
		t.Fatalf("zero reprice: %+v %v", rows, err)
	}
	if err := db.Close(ctx); err != nil {
		t.Fatal(err)
	}
	db = openDiscoveryStore(t, path, fetcher, &now)
	book, err = db.PriceBook(ctx)
	if err != nil || book.KiroUSDPerCredit == nil || *book.KiroUSDPerCredit != 0 {
		t.Fatalf("zero restart: %+v %v", book, err)
	}
}

func TestCreditMigrationKeepsOldEventsUnmetered(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.db")
	db, err := Open(ctx, Config{Path: path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	event := v2Event(strings.Repeat("5", 32), strings.Repeat("6", 32), strings.Repeat("a", 64), start.Add(time.Minute), true, nil, 0, 0, 10, 5)
	if err = db.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.ExecContext(ctx, `ALTER TABLE events DROP COLUMN cancellation_source; ALTER TABLE events DROP COLUMN failure_hop; ALTER TABLE events DROP COLUMN outcome; DROP INDEX IF EXISTS events_session_id_idx; ALTER TABLE events DROP COLUMN session_id; ALTER TABLE events DROP COLUMN pricing_breakdown; ALTER TABLE events DROP COLUMN provider_credits; ALTER TABLE events DROP COLUMN price_usd_per_credit_nano; ALTER TABLE events DROP COLUMN routing_trace_json; DELETE FROM schema_migrations WHERE version>=16`)
	if err != nil {
		t.Fatal(err)
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, Config{Path: path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(ctx) }()
	page, err := db.Events(ctx, v2Query(model.OperationEvents, start, start.Add(time.Hour)))
	if err != nil || len(page.Events) != 1 || page.Events[0].ProviderCredits != nil || page.Events[0].PriceUSDPerCredit != nil {
		t.Fatalf("migrated: %+v %v", page, err)
	}
}

func TestCreditImportBackupAndMixedAnalysis(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rate := model.NanoUSD(1_000_000_000)
	db, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "token", Provider: "codex", Model: "model-v2", InputPerMillion: &rate, OutputPerMillion: &rate, Source: "test"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(ctx) }()
	credit := "2"
	forged := model.NanoUSD(1)
	imported := v2Event(strings.Repeat("7", 32), strings.Repeat("8", 32), strings.Repeat("a", 64), start.Add(time.Minute), true, nil, 0, 0, 10, 5)
	imported.Provider = "kiro"
	imported.ProviderCredits = &credit
	imported.KnownCost = &forged
	imported.PriceUSDPerCredit = &forged
	token := v2Event(strings.Repeat("9", 32), strings.Repeat("b", 32), strings.Repeat("a", 64), start.Add(2*time.Minute), true, nil, 0, 0, 10, 5)
	token.Provider = "codex"
	if n, err := db.WriteImportBatch(ctx, []model.Event{imported}, "credit-import"); err != nil || n != 1 {
		t.Fatalf("import: %d %v", n, err)
	}
	if err = db.WriteBatch(ctx, []model.Event{token}); err != nil {
		t.Fatal(err)
	}
	query := v2Query(model.OperationAnalysis, start, start.Add(time.Hour))
	components, err := db.analysisCosts(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	total, err := sumComponentUSD(components.CreditBasedUSD, components.UncachedInputUSD, components.CacheReadUSD, components.CacheCreationUSD, components.OutputUSD)
	if err != nil {
		t.Fatal(err)
	}
	var stored int64
	if err = db.db.QueryRowContext(ctx, "SELECT SUM(known_cost_nano) FROM events").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if components.CreditBasedUSD != "0.04" || total != model.NanoUSD(stored) {
		t.Fatalf("mixed: %+v total=%d stored=%d", components, total, stored)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if _, err = db.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if err = db.Restore(ctx, backup, backup+".manifest.json"); err != nil {
		t.Fatal(err)
	}
	found, ok, err := db.EventByAttemptID(ctx, imported.AttemptID, v2Query(model.OperationEvents, start, start.Add(time.Hour)))
	if err != nil || !ok || found.ProviderCredits == nil || *found.ProviderCredits != credit || found.PriceUSDPerCredit == nil || *found.PriceUSDPerCredit != 20_000_000 || found.KnownCost == nil || *found.KnownCost != 40_000_000 {
		t.Fatalf("roundtrip: %+v %v %v", found, ok, err)
	}
}

func TestCreditRateInvalidatesRepriceCheckpoint(t *testing.T) {
	a, err := pricingCatalogDigest(aggregate.PriceBook{})
	if err != nil {
		t.Fatal(err)
	}
	zero := model.NanoUSD(0)
	b, err := pricingCatalogDigest(aggregate.PriceBook{KiroUSDPerCredit: &zero})
	if err != nil || a == b {
		t.Fatalf("digest did not change: %s %s %v", a, b, err)
	}
}

func TestInvalidCreditBatchRollsBack(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(ctx) }()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	valid := v2Event(strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("a", 64), start, true, nil, 0, 0, 10, 5)
	invalid := valid
	invalid.AttemptID = strings.Repeat("3", 32)
	malformed := "1e999999"
	invalid.ProviderCredits = &malformed
	if _, err = db.WriteImportBatch(ctx, []model.Event{valid, invalid}, "invalid-credit-import"); err == nil {
		t.Fatal("invalid import batch accepted")
	}
	var count int
	if err = db.db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&count); err != nil || count != 0 {
		t.Fatalf("batch was not atomic: %d %v", count, err)
	}
}
