package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

type testPricingFetcher struct {
	mu      sync.Mutex
	calls   int
	catalog PricingCatalog
	err     error
	started chan struct{}
	release chan struct{}
}

func (f *testPricingFetcher) Fetch(ctx context.Context) (PricingCatalog, error) {
	f.mu.Lock()
	f.calls++
	started, release := f.started, f.release
	catalog, err := f.catalog, f.err
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		case <-ctx.Done():
			return PricingCatalog{}, ctx.Err()
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return PricingCatalog{}, ctx.Err()
		}
	}
	return catalog, err
}

func (f *testPricingFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func testPricingCatalog(input, output model.NanoUSD) PricingCatalog {
	return PricingCatalog{Digest: strings.Repeat("a", 64), Rules: []aggregate.PricingRule{{
		Provider: "codex", ID: "models.dev:codex:gpt-5", Model: "gpt-5",
		InputPerMillion: &input, OutputPerMillion: &output,
		CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: ModelsDevSource, Catalog: true,
	}}}
}

func openDiscoveryStore(t *testing.T, path string, fetcher PricingFetcher, now *time.Time) *SQLiteStore {
	t.Helper()
	database, err := Open(context.Background(), Config{Path: path, MaxStorageBytes: 64 << 20,
		PricingFetcher: fetcher, PricingNow: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	return database
}

func TestPricingDiscoveryIsLazyAndUsesSixHourTTL(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	input, output := model.NanoUSD(1_000_000_000), model.NanoUSD(2_000_000_000)
	fetcher := &testPricingFetcher{catalog: testPricingCatalog(input, output)}
	database := openDiscoveryStore(t, filepath.Join(t.TempDir(), "analytics.db"), fetcher, &now)
	if calls := fetcher.count(); calls != 0 {
		t.Fatalf("startup fetch calls = %d, want 0", calls)
	}
	result, err := database.RefreshPricing(context.Background())
	if err != nil || result.State != "fresh" || fetcher.count() != 1 {
		t.Fatalf("first refresh state=%q err=%v calls=%d", result.State, err, fetcher.count())
	}
	if _, err := database.RefreshPricing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := fetcher.count(); calls != 1 {
		t.Fatalf("fresh refresh calls = %d, want 1", calls)
	}
	now = now.Add(PricingCatalogTTL + time.Nanosecond)
	if _, err := database.RefreshPricing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := fetcher.count(); calls != 2 {
		t.Fatalf("expired refresh calls = %d, want 2", calls)
	}
}

func TestPricingDiscoveryCoalescesConcurrentDemand(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	input, output := model.NanoUSD(1), model.NanoUSD(2)
	started, release := make(chan struct{}, 1), make(chan struct{})
	fetcher := &testPricingFetcher{catalog: testPricingCatalog(input, output), started: started, release: release}
	database := openDiscoveryStore(t, filepath.Join(t.TempDir(), "analytics.db"), fetcher, &now)
	results := make(chan error, 2)
	go func() { _, err := database.RefreshPricing(context.Background()); results <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first refresh did not start")
	}
	go func() { _, err := database.RefreshPricing(context.Background()); results <- err }()
	select {
	case <-release:
		t.Fatal("release channel unexpectedly consumed")
	default:
	}
	close(release)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("coalesced refresh did not complete")
		}
	}
	if calls := fetcher.count(); calls != 1 {
		t.Fatalf("concurrent fetch calls = %d, want 1", calls)
	}
}

func TestPricingDiscoveryKeepsStaleCatalogAndSuppressesRetry(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	input, output := model.NanoUSD(1), model.NanoUSD(2)
	fetcher := &testPricingFetcher{catalog: testPricingCatalog(input, output)}
	database := openDiscoveryStore(t, filepath.Join(t.TempDir(), "analytics.db"), fetcher, &now)
	if _, err := database.RefreshPricing(context.Background()); err != nil {
		t.Fatal(err)
	}
	fetcher.mu.Lock()
	fetcher.err = errors.New("upstream unavailable")
	fetcher.mu.Unlock()
	now = now.Add(PricingCatalogTTL + time.Nanosecond)
	result, err := database.RefreshPricing(context.Background())
	if err == nil || result.State != "stale" || len(result.Snapshot.Catalog) != 1 {
		t.Fatalf("stale result state=%q catalog=%d err=%v", result.State, len(result.Snapshot.Catalog), err)
	}
	if _, err := database.RefreshPricing(context.Background()); err != nil {
		// Retry suppression returns the stale snapshot without surfacing the
		// previous transport error to each caller.
		t.Fatal(err)
	}
	if calls := fetcher.count(); calls != 2 {
		t.Fatalf("suppressed retry calls = %d, want 2", calls)
	}
}

func TestPricingDiscoveryPersistsAndManualOverridesSurviveRefresh(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	input, output := model.NanoUSD(1), model.NanoUSD(2)
	path := filepath.Join(t.TempDir(), "analytics.db")
	fetcher := &testPricingFetcher{catalog: testPricingCatalog(input, output)}
	database := openDiscoveryStore(t, path, fetcher, &now)
	if _, err := database.RefreshPricing(ctx); err != nil {
		t.Fatal(err)
	}
	manualInput, manualOutput := model.NanoUSD(9), model.NanoUSD(9)
	if _, err := database.UpdatePriceBook(ctx, aggregate.PriceBook{Rules: []aggregate.PricingRule{{
		Provider: "codex", ID: "manual-alias", Alias: "billing-gpt", InputPerMillion: &manualInput,
		OutputPerMillion: &manualOutput, Source: "operator", CacheReadMultiplier: "1", CacheCreationMultiplier: "1",
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}
	fetcher2 := &testPricingFetcher{catalog: testPricingCatalog(input, output)}
	database = openDiscoveryStore(t, path, fetcher2, &now)
	snapshot, err := database.PricingSnapshot(ctx)
	if err != nil || len(snapshot.Catalog) != 1 || len(snapshot.Overrides) != 1 {
		t.Fatalf("restart snapshot catalog=%d overrides=%d err=%v", len(snapshot.Catalog), len(snapshot.Overrides), err)
	}
	book, err := database.PriceBook(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alias := "billing-gpt"
	priced, err := book.Price(model.Event{Provider: "codex", Model: "gpt-5", RequestedAlias: &alias, Tokens: model.TokenUsage{Input: 1, Output: 1, Total: 2}})
	if err != nil || priced.RuleID != "manual-alias" {
		t.Fatalf("manual alias result=%+v err=%v", priced, err)
	}
	now = now.Add(PricingCatalogTTL + time.Nanosecond)
	if _, err := database.RefreshPricing(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err = database.PricingSnapshot(ctx)
	if err != nil || len(snapshot.Catalog) != 1 || len(snapshot.Overrides) != 1 {
		t.Fatalf("post-refresh snapshot catalog=%d overrides=%d err=%v", len(snapshot.Catalog), len(snapshot.Overrides), err)
	}
}

func TestPricingDiscoveryRefetchesWhenBindingsChangeDuringFetch(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	input, output := model.NanoUSD(1), model.NanoUSD(2)
	started, release := make(chan struct{}, 1), make(chan struct{})
	catalog := testPricingCatalog(input, output)
	catalog.BindingsDigest = catalogBindingsDigest(nil)
	fetcher := &testPricingFetcher{catalog: catalog, started: started, release: release}
	database := openDiscoveryStore(t, filepath.Join(t.TempDir(), "analytics.db"), fetcher, &now)
	results := make(chan error, 1)
	go func() { _, err := database.RefreshPricing(context.Background()); results <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first refresh did not start")
	}
	// A reconfigure lands while the fetch built from the old bindings is in flight.
	database.SetCatalogBindings([]CatalogBinding{{Provider: "openai-compatible-zai", Catalog: "zai"}})
	close(release)
	select {
	case err := <-results:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh did not complete")
	}
	fetcher.mu.Lock()
	fetcher.started, fetcher.release = nil, nil
	fetcher.mu.Unlock()
	if _, err := database.RefreshPricing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := fetcher.count(); calls != 2 {
		t.Fatalf("fetch calls after in-flight binding change = %d, want 2", calls)
	}
}
