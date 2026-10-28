package chstore

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

type c3Fetcher struct {
	mu      sync.Mutex
	calls   int
	catalog PricingCatalog
}

func (f *c3Fetcher) Fetch(context.Context) (PricingCatalog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.catalog, nil
}
func TestC3PricingRefreshAndReprice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rate := model.NanoUSD(1_000_000_000)
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	f := &c3Fetcher{catalog: PricingCatalog{Digest: strings.Repeat("a", 64), Rules: []aggregate.PricingRule{{ID: "models.dev:codex:gpt-5", Provider: "codex", Model: "gpt-5", InputPerMillion: &rate, OutputPerMillion: &rate, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: ModelsDevSource, Catalog: true}}, Providers: []CatalogProvider{{ID: "openai", Name: "OpenAI"}}}}
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PricingFetcher: f, PricingNow: func() time.Time { return now }})
	result, err := s.RefreshPricing(ctx)
	if err != nil || result.State != "fresh" || f.calls != 1 {
		t.Fatalf("refresh: %+v %v", result, err)
	}
	_, err = s.RefreshPricing(ctx)
	if err != nil || f.calls != 1 {
		t.Fatalf("fresh refresh: %v calls=%d", err, f.calls)
	}
	providers, at, err := s.CatalogProviders(ctx)
	if err != nil || len(providers) != 1 || !at.Equal(now) {
		t.Fatalf("providers: %+v %v %v", providers, at, err)
	}
	events := loadFixtureEvents(t)
	if err := s.WriteBatch(ctx, events[:2]); err != nil {
		t.Fatal(err)
	}
	missing, err := s.PricingMissing(ctx, model.Range{Start: events[0].RequestedAt.Add(-24 * time.Hour), End: events[0].RequestedAt.Add(48 * time.Hour), TimeZone: "UTC"})
	if err != nil {
		t.Fatalf("pricing missing: %v", err)
	}
	if len(missing) == 0 {
		t.Fatal("expected missing pricing")
	}
	book := fixturePriceBook()
	if _, err := s.UpdatePriceBook(ctx, book); err != nil {
		t.Fatal(err)
	}
	start, end := events[0].RequestedAt, events[0].RequestedAt
	for _, e := range events[:2] {
		if e.RequestedAt.Before(start) {
			start = e.RequestedAt
		}
		if e.RequestedAt.After(end) {
			end = e.RequestedAt
		}
	}
	opts := RepriceOptions{Range: model.Range{Start: start, End: end.Add(time.Nanosecond), TimeZone: "UTC"}, ChunkSize: 1}
	first, err := s.Reprice(ctx, opts, nil)
	if err != nil || first.Updated != 1 || first.Completed {
		t.Fatalf("first chunk %+v %v", first, err)
	}
	opts.Resume = true
	second, err := s.Reprice(ctx, opts, nil)
	if err != nil || second.Updated != 1 || !second.Completed {
		t.Fatalf("second chunk %+v %v", second, err)
	}
	loaded, err := s.loadEvents(ctx, "attempt_id IN (?)", []string{events[0].AttemptID, events[1].AttemptID})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range loaded {
		expected, err := book.Price(e)
		if err != nil {
			t.Fatal(err)
		}
		if (e.KnownCost == nil) != (expected.KnownCost == nil) || e.KnownCost != nil && *e.KnownCost != *expected.KnownCost || e.UnpricedTokens != expected.UnpricedTokens {
			t.Fatalf("repriced event %+v expected %+v", e, expected)
		}
	}
}
func TestC3QuotaHistoryAndLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	id := strings.Repeat("a", 64)
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	sample := QuotaHistorySample{Provider: "codex", CredentialID: id, WindowID: "daily", RemainingPercent: 50, Source: "cpamc"}
	if err := s.RecordQuotaHistory(ctx, []QuotaHistorySample{sample}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordQuotaHistory(ctx, []QuotaHistorySample{sample}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	history, err := s.QuotaHistory(ctx, "codex", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(history) != 1 {
		t.Fatalf("history %+v %v", history, err)
	}
	if err := s.UpdateKeyLifecycle(ctx, []string{id}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateKeyLifecycle(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	values, err := s.loadControl(ctx, "key_lifecycle")
	if err != nil || !strings.Contains(string(values[id]), "deleted") {
		t.Fatalf("lifecycle %s %v", values[id], err)
	}
	quota := ProviderQuotaSnapshot{Provider: "codex", CredentialID: id, ObservedAt: now, Available: true}
	if err := s.ReplaceProviderQuotaSnapshots(ctx, []ProviderQuotaSnapshot{quota}); err != nil {
		t.Fatal(err)
	}
	snapshots, err := s.ProviderQuotaSnapshots(ctx)
	if err != nil || len(snapshots) != 1 || !snapshots[0].ObservedAt.Equal(now) {
		t.Fatalf("snapshots %+v %v", snapshots, err)
	}
	credentials, err := s.ProviderCredentials(ctx)
	if err != nil || len(credentials) != 1 || credentials[0].CredentialID != id {
		t.Fatalf("credentials %+v %v", credentials, err)
	}
}

func TestC3RepriceMatchesSQLite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := loadFixtureEvents(t)
	ch := openTestStore(t)
	sqlite, err := store.Open(ctx, store.Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlite.Close(ctx) }()
	if err = ch.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	if err = sqlite.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	book := fixturePriceBook()
	if _, err = ch.UpdatePriceBook(ctx, book); err != nil {
		t.Fatal(err)
	}
	if _, err = sqlite.UpdatePriceBook(ctx, book); err != nil {
		t.Fatal(err)
	}
	start, end := events[0].RequestedAt, events[0].RequestedAt
	for _, event := range events {
		if event.RequestedAt.Before(start) {
			start = event.RequestedAt
		}
		if event.RequestedAt.After(end) {
			end = event.RequestedAt
		}
	}
	start = start.Add(-time.Second)
	end = end.Add(time.Second)
	options := RepriceOptions{Range: model.Range{Start: start, End: end, TimeZone: "UTC"}, ChunkSize: 100}
	a, err := ch.Reprice(ctx, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := sqlite.Reprice(ctx, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Matched != b.Matched || a.Updated != b.Updated {
		t.Fatalf("counts clickhouse=%+v sqlite=%+v", a, b)
	}
	query := model.Query{SchemaVersion: 1, Operation: model.OperationEvents, Start: start, End: end, TimeZone: "UTC", PageSize: 100}
	page, err := sqlite.Events(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ch.loadEvents(ctx, "requested_at_ns>=? AND requested_at_ns<?", start.UnixNano(), end.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]model.Event{}
	for _, e := range page.Events {
		expected[e.AttemptID] = e
	}
	for _, e := range got {
		want, ok := expected[e.AttemptID]
		if !ok || (e.KnownCost == nil) != (want.KnownCost == nil) || e.KnownCost != nil && *e.KnownCost != *want.KnownCost || e.UnpricedTokens != want.UnpricedTokens || e.PriceRuleID != want.PriceRuleID {
			t.Fatalf("attempt %s CH=%+v SQLite=%+v", e.AttemptID, e, want)
		}
	}
	again, err := ch.Reprice(ctx, options, nil)
	if err != nil || again.Updated != a.Updated {
		t.Fatalf("repeat %+v %v", again, err)
	}
}

type c3BlockingFetcher struct {
	started chan struct{}
	release chan struct{}
	calls   int
	mu      sync.Mutex
	catalog PricingCatalog
}

func (f *c3BlockingFetcher) Fetch(ctx context.Context) (PricingCatalog, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	close(f.started)
	select {
	case <-f.release:
		return f.catalog, nil
	case <-ctx.Done():
		return PricingCatalog{}, ctx.Err()
	}
}
func TestC3RefreshSingleFlight(t *testing.T) {
	t.Parallel()
	rate := model.NanoUSD(1000000000)
	f := &c3BlockingFetcher{started: make(chan struct{}), release: make(chan struct{}), catalog: PricingCatalog{Digest: strings.Repeat("b", 64), Rules: []aggregate.PricingRule{{ID: "models.dev:codex:gpt-5", Provider: "codex", Model: "gpt-5", InputPerMillion: &rate, OutputPerMillion: &rate, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: ModelsDevSource, Catalog: true}}}}
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PricingFetcher: f})
	ctx := context.Background()
	done := make(chan error, 2)
	go func() { _, err := s.RefreshPricing(ctx); done <- err }()
	<-f.started
	go func() { _, err := s.RefreshPricing(ctx); done <- err }()
	close(f.release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls != 1 {
		t.Fatalf("fetch calls = %d", f.calls)
	}
}
