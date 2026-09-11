package cpauk

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
)

type serviceBindingFetcher struct {
	mu    sync.Mutex
	calls int
}

func (f *serviceBindingFetcher) Fetch(context.Context) (store.PricingCatalog, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	input, output := model.NanoUSD(1), model.NanoUSD(2)
	return store.PricingCatalog{Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Rules: []aggregate.PricingRule{{
		Provider: "codex", ID: "models.dev:codex:gpt-5", Model: "gpt-5", InputPerMillion: &input, OutputPerMillion: &output,
		CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: store.ModelsDevSource, Catalog: true,
	}}}, nil
}

func (f *serviceBindingFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type servicePricingBackend struct {
	*fakeBackend
	calls        atomic.Int64
	refreshFunc  func(int64) (store.PricingRefreshResult, error)
	panicRefresh atomic.Bool
}

func (b *servicePricingBackend) RefreshPricing(_ context.Context) (store.PricingRefreshResult, error) {
	call := b.calls.Add(1)
	if b.panicRefresh.Load() {
		panic("injected pricing refresh panic")
	}
	if b.refreshFunc == nil {
		return store.PricingRefreshResult{}, nil
	}
	return b.refreshFunc(call)
}

func TestPricingDemandSkipsFreshCatalogUntilExpiry(t *testing.T) {
	firstExpiry := time.Now().Add(150 * time.Millisecond)
	backend := &servicePricingBackend{fakeBackend: &fakeBackend{}}
	backend.refreshFunc = func(call int64) (store.PricingRefreshResult, error) {
		expiresAt := firstExpiry
		if call > 1 {
			expiresAt = time.Now().Add(time.Hour)
		}
		return store.PricingRefreshResult{State: "fresh", Snapshot: store.PricingSnapshot{
			Provenance: store.PricingProvenance{CatalogExpiresAt: expiresAt},
		}}, nil
	}
	serviceFacade, serviceBackend := newPricingDemandService(t, backend)

	serviceFacade.requestPricingRefresh()
	waitPricingRefreshCalls(t, serviceBackend, 1)
	for time.Now().Before(firstExpiry) {
		serviceFacade.requestPricingRefresh()
		time.Sleep(time.Millisecond)
	}
	if calls := serviceBackend.calls.Load(); calls != 1 {
		t.Fatalf("fresh pricing refresh calls before expiry = %d, want 1", calls)
	}

	serviceFacade.requestPricingRefresh()
	waitPricingRefreshCalls(t, serviceBackend, 2)
}

func TestPricingDemandSuppressesFailedRefreshRetry(t *testing.T) {
	retryAt := time.Now().Add(150 * time.Millisecond)
	backend := &servicePricingBackend{fakeBackend: &fakeBackend{}}
	backend.refreshFunc = func(call int64) (store.PricingRefreshResult, error) {
		if call == 1 {
			return store.PricingRefreshResult{Snapshot: store.PricingSnapshot{
				Provenance: store.PricingProvenance{CatalogRetryAt: retryAt},
			}}, errors.New("upstream unavailable")
		}
		return store.PricingRefreshResult{State: "fresh", Snapshot: store.PricingSnapshot{
			Provenance: store.PricingProvenance{CatalogExpiresAt: time.Now().Add(time.Hour)},
		}}, nil
	}
	serviceFacade, serviceBackend := newPricingDemandService(t, backend)

	serviceFacade.requestPricingRefresh()
	waitPricingRefreshCalls(t, serviceBackend, 1)
	for time.Now().Before(retryAt) {
		serviceFacade.requestPricingRefresh()
		time.Sleep(time.Millisecond)
	}
	if calls := serviceBackend.calls.Load(); calls != 1 {
		t.Fatalf("failed pricing refresh calls before retry time = %d, want 1", calls)
	}

	serviceFacade.requestPricingRefresh()
	waitPricingRefreshCalls(t, serviceBackend, 2)
}

func TestPricingDemandPanicIsRecoveredAndSuppressed(t *testing.T) {
	backend := &servicePricingBackend{fakeBackend: &fakeBackend{}}
	backend.panicRefresh.Store(true)
	serviceFacade, serviceBackend := newPricingDemandService(t, backend)

	serviceFacade.requestPricingRefresh()
	waitPricingRefreshCalls(t, serviceBackend, 1)
	for index := 0; index < 20; index++ {
		serviceFacade.requestPricingRefresh()
	}
	time.Sleep(20 * time.Millisecond)
	if calls := serviceBackend.calls.Load(); calls != 1 {
		t.Fatalf("panic pricing refresh calls = %d, want 1", calls)
	}
}

func TestPricingDemandInvalidatesOnReconfigure(t *testing.T) {
	backend := &servicePricingBackend{fakeBackend: &fakeBackend{}}
	backend.refreshFunc = func(int64) (store.PricingRefreshResult, error) {
		return store.PricingRefreshResult{State: "fresh", Snapshot: store.PricingSnapshot{
			Provenance: store.PricingProvenance{CatalogExpiresAt: time.Now().Add(time.Hour)},
		}}, nil
	}
	serviceFacade, serviceBackend := newPricingDemandService(t, backend)

	serviceFacade.requestPricingRefresh()
	waitPricingRefreshCalls(t, serviceBackend, 1)
	config := smallConfig()
	config.BatchSize = 2
	if result := serviceFacade.Reconfigure(config); !result.Applied || result.Error != nil {
		t.Fatalf("reconfigure = %#v", result)
	}
	serviceFacade.requestPricingRefresh()
	waitPricingRefreshCalls(t, serviceBackend, 2)
}

func TestPricingReconfigureCatalogBindingsTriggersRefresh(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	fetcher := &serviceBindingFetcher{}
	config := smallConfig()
	config.Path = filepath.Join(t.TempDir(), "analytics.db")
	config.CatalogBindings = []CatalogBinding{{Provider: "openai-compatible-zai", Catalog: "zai"}}
	serviceFacade := New(context.Background(), config, func(ctx context.Context, cfg Config) (Backend, [32]byte, error) {
		bindings := make([]store.CatalogBinding, 0, len(cfg.CatalogBindings))
		for _, binding := range cfg.CatalogBindings {
			bindings = append(bindings, store.CatalogBinding{Provider: binding.Provider, Catalog: binding.Catalog})
		}
		backend, err := store.Open(ctx, store.Config{Path: cfg.Path, MaxStorageBytes: cfg.MaxStorageBytes,
			PricingFetcher: fetcher, PricingNow: func() time.Time { return now }, CatalogBindings: bindings})
		return backend, [32]byte{1}, err
	})
	t.Cleanup(func() { _ = serviceFacade.Close(context.Background()) })
	waitServiceState(t, serviceFacade, StateReady)
	service := serviceFacade.(*service)
	if _, err := service.RefreshPricing(context.Background()); err != nil {
		t.Fatal(err)
	}
	updated := config
	updated.CatalogBindings = []CatalogBinding{{Provider: "openai-compatible-groq", Catalog: "groq"}}
	result := service.Reconfigure(updated)
	if !result.Applied || result.Error != nil || len(result.RestartRequired) != 0 {
		t.Fatalf("binding reconfigure=%+v", result)
	}
	if _, err := service.RefreshPricing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := fetcher.count(); calls != 2 {
		t.Fatalf("refresh calls after binding change=%d, want 2", calls)
	}
}

func newPricingDemandService(t *testing.T, backend *servicePricingBackend) (*service, *servicePricingBackend) {
	t.Helper()
	serviceFacade := New(context.Background(), smallConfig(), func(context.Context, Config) (Backend, [32]byte, error) {
		return backend, [32]byte{1}, nil
	})
	t.Cleanup(func() { _ = serviceFacade.Close(context.Background()) })
	waitServiceState(t, serviceFacade, StateReady)
	service, ok := serviceFacade.(*service)
	if !ok {
		t.Fatalf("service type = %T", serviceFacade)
	}
	return service, backend
}

func waitPricingRefreshCalls(t *testing.T, backend *servicePricingBackend, want int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for backend.calls.Load() < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls := backend.calls.Load(); calls < want {
		t.Fatalf("pricing refresh calls = %d, want at least %d", calls, want)
	}
}
