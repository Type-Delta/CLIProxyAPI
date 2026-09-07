package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
	"golang.org/x/crypto/bcrypt"
)

type integrationPricingFetcher struct {
	catalog store.PricingCatalog
	err     error
	calls   atomic.Int64
	started chan struct{}
	release chan struct{}
}

func (f *integrationPricingFetcher) Fetch(ctx context.Context) (store.PricingCatalog, error) {
	f.calls.Add(1)
	select {
	case f.started <- struct{}{}:
	case <-ctx.Done():
		return store.PricingCatalog{}, ctx.Err()
	}
	select {
	case <-f.release:
		if f.err != nil {
			return store.PricingCatalog{}, f.err
		}
		return f.catalog, nil
	case <-ctx.Done():
		return store.PricingCatalog{}, ctx.Err()
	}
}

type integrationPricingResponse struct {
	SyncState        string                   `json:"sync_state"`
	CatalogUpdatedAt *time.Time               `json:"catalog_updated_at"`
	Rules            []integrationPricingRule `json:"rules"`
	Catalog          []integrationPricingRule `json:"catalog"`
	Overrides        []integrationPricingRule `json:"overrides"`
}

type integrationPricingRule struct {
	RuleID string `json:"rule_id"`
	Match  struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Alias    string `json:"alias"`
	} `json:"match"`
	InputPerMillion  string     `json:"input_per_million_usd"`
	OutputPerMillion string     `json:"output_per_million_usd"`
	Source           string     `json:"source"`
	UpdatedAt        *time.Time `json:"updated_at"`
}

func TestPricingDiscoveryThroughManagementAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	input, output := model.NanoUSD(1), model.NanoUSD(2)
	fetcher := &integrationPricingFetcher{
		catalog: store.PricingCatalog{
			Digest: strings.Repeat("a", 64),
			Rules: []aggregate.PricingRule{{
				Provider: "codex", ID: "models.dev:codex:gpt-5", Model: "gpt-5",
				InputPerMillion: &input, OutputPerMillion: &output,
				CacheReadMultiplier: "1", CacheCreationMultiplier: "1",
				Source: store.ModelsDevSource, Catalog: true,
			}},
		},
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}

	server, _ := newPricingManagementServer(t, fetcher)
	if calls := fetcher.calls.Load(); calls != 0 {
		t.Fatalf("idle startup fetch calls = %d, want 0", calls)
	}

	secret := "management-pricing-integration-secret"
	initial := pricingManagementRequest(t, server, secret, http.MethodGet, "/v0/management/analytics/pricing", "")
	select {
	case <-fetcher.started:
	case <-time.After(time.Second):
		t.Fatal("management GET did not trigger pricing discovery")
	}
	if initial.SyncState != "refreshing" {
		t.Fatalf("initial sync state = %q, want refreshing", initial.SyncState)
	}
	if calls := fetcher.calls.Load(); calls != 1 {
		t.Fatalf("first GET fetch calls = %d, want 1", calls)
	}

	close(fetcher.release)
	ready := waitForCatalogResponse(t, server, secret)
	if ready.SyncState != "ready" || len(ready.Catalog) != 1 {
		t.Fatalf("ready pricing response = %+v", ready)
	}
	if ready.Catalog[0].RuleID != "models.dev:codex:gpt-5" || ready.Catalog[0].InputPerMillion != "0.000000001" {
		t.Fatalf("catalog rule = %+v", ready.Catalog[0])
	}
	if ready.CatalogUpdatedAt == nil || ready.Catalog[0].UpdatedAt == nil {
		t.Fatalf("catalog timestamps = response %s, rule %s", ready.CatalogUpdatedAt, ready.Catalog[0].UpdatedAt)
	}
	if calls := fetcher.calls.Load(); calls != 1 {
		t.Fatalf("fresh catalog fetch calls = %d, want 1", calls)
	}
	_ = pricingManagementRequest(t, server, secret, http.MethodGet, "/v0/management/analytics/pricing", "")
	if calls := fetcher.calls.Load(); calls != 1 {
		t.Fatalf("fresh follow-up GET fetch calls = %d, want 1", calls)
	}

	alias := pricingManagementRequest(t, server, secret, http.MethodPut, "/v0/management/analytics/pricing", `{"rules":[{"rule_id":"operator-alias","match":{"provider":"codex","alias":"gpt-5"},"input_per_million_usd":"9","output_per_million_usd":"10","cache_read_multiplier":"1","cache_creation_multiplier":"1","source":"operator"}]}`)
	if len(alias.Overrides) != 1 || len(alias.Catalog) != 1 || len(alias.Rules) != 2 {
		t.Fatalf("alias response = %+v", alias)
	}
	if alias.Overrides[0].UpdatedAt == nil || alias.Catalog[0].UpdatedAt == nil {
		t.Fatalf("alias rule timestamps = %+v", alias)
	}
	if alias.Catalog[0].RuleID != "models.dev:codex:gpt-5" || alias.Overrides[0].RuleID != "operator-alias" {
		t.Fatalf("alias response rows = %+v", alias)
	}

	overridden := pricingManagementRequest(t, server, secret, http.MethodPut, "/v0/management/analytics/pricing", `{"rules":[{"rule_id":"operator-gpt-5","match":{"provider":"codex","model":"gpt-5"},"input_per_million_usd":"9","output_per_million_usd":"10","cache_read_multiplier":"1","cache_creation_multiplier":"1","source":"operator"}]}`)
	if len(overridden.Overrides) != 1 || len(overridden.Catalog) != 1 || len(overridden.Rules) != 1 {
		t.Fatalf("override response = %+v", overridden)
	}
	if overridden.Rules[0].RuleID != "operator-gpt-5" || overridden.Rules[0].InputPerMillion != "9" {
		t.Fatalf("effective override rule = %+v", overridden.Rules[0])
	}
	if calls := fetcher.calls.Load(); calls != 1 {
		t.Fatalf("override PUT fetch calls = %d, want 1", calls)
	}

	restored := pricingManagementRequest(t, server, secret, http.MethodPut, "/v0/management/analytics/pricing", `{"rules":[]}`)
	if len(restored.Overrides) != 0 || len(restored.Rules) != 1 || restored.Rules[0].RuleID != "models.dev:codex:gpt-5" || restored.Rules[0].InputPerMillion != "0.000000001" {
		t.Fatalf("restored catalog response = %+v", restored)
	}
	if calls := fetcher.calls.Load(); calls != 1 {
		t.Fatalf("override deletion fetch calls = %d, want 1", calls)
	}
}

func TestPricingDiscoveryFailureBackoffThroughManagementAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fetcher := &integrationPricingFetcher{
		err:     errors.New("models.dev unavailable"),
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	server, _ := newPricingManagementServer(t, fetcher)
	secret := "management-pricing-integration-secret"

	initial := pricingManagementRequest(t, server, secret, http.MethodGet, "/v0/management/analytics/pricing", "")
	select {
	case <-fetcher.started:
	case <-time.After(time.Second):
		t.Fatal("management GET did not trigger failed pricing discovery")
	}
	if initial.SyncState != "refreshing" {
		t.Fatalf("initial failure sync state = %q, want refreshing", initial.SyncState)
	}
	close(fetcher.release)

	failed := waitForPricingSyncState(t, server, secret, "unavailable")
	if len(failed.Catalog) != 0 || len(failed.Rules) != 0 {
		t.Fatalf("failed pricing response = %+v", failed)
	}
	if calls := fetcher.calls.Load(); calls != 1 {
		t.Fatalf("failed refresh fetch calls = %d, want 1", calls)
	}
	again := pricingManagementRequest(t, server, secret, http.MethodGet, "/v0/management/analytics/pricing", "")
	if again.SyncState != "unavailable" {
		t.Fatalf("backoff sync state = %q, want unavailable", again.SyncState)
	}
	if calls := fetcher.calls.Load(); calls != 1 {
		t.Fatalf("backoff fetch calls = %d, want 1", calls)
	}
}

func newPricingManagementServer(t *testing.T, fetcher *integrationPricingFetcher) (*Server, cpauk.Service) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "analytics.db")
	serviceConfig := cpauk.DefaultConfig()
	serviceConfig.Enabled = true
	serviceConfig.Path = path
	serviceConfig.QueueCapacity = 8
	serviceConfig.BatchSize = 1
	serviceConfig.FlushInterval = time.Millisecond
	serviceConfig.MaxStorageBytes = 64 << 20
	serviceConfig.MinFreeBytes = 0
	serviceConfig.ShutdownDrain = time.Second
	service := cpauk.New(context.Background(), serviceConfig, func(factoryContext context.Context, _ cpauk.Config) (cpauk.Backend, [32]byte, error) {
		database, err := store.Open(factoryContext, store.Config{
			Path: path, MaxStorageBytes: 64 << 20, PricingFetcher: fetcher,
		})
		if err != nil {
			return nil, [32]byte{}, err
		}
		return database, database.IdentityKeyArray(), nil
	})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	waitForPricingServiceReady(t, service)

	secret := "management-pricing-integration-secret"
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{RemoteManagement: config.RemoteManagement{AllowRemote: true, SecretKey: string(hash)}}
	handler := managementHandlers.NewHandlerWithoutConfigFilePath(cfg, nil)
	handler.SetAnalyticsService(service)
	server := &Server{engine: gin.New(), cfg: cfg, mgmt: handler, analytics: service}
	server.managementRoutesEnabled.Store(true)
	server.registerManagementRoutes()
	return server, service
}

func waitForPricingServiceReady(t *testing.T, service cpauk.Service) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if service.Capabilities().State == cpauk.StateReady {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("analytics service did not become ready: %+v", service.Health())
}

func waitForCatalogResponse(t *testing.T, server *Server, secret string) integrationPricingResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response := pricingManagementRequest(t, server, secret, http.MethodGet, "/v0/management/analytics/pricing", "")
		if response.SyncState == "ready" && len(response.Catalog) != 0 {
			return response
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("pricing catalog did not become ready")
	return integrationPricingResponse{}
}

func waitForPricingSyncState(t *testing.T, server *Server, secret, wanted string) integrationPricingResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response := pricingManagementRequest(t, server, secret, http.MethodGet, "/v0/management/analytics/pricing", "")
		if response.SyncState == wanted {
			return response
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pricing sync state did not become %q", wanted)
	return integrationPricingResponse{}
}

func pricingManagementRequest(t *testing.T, server *Server, secret, method, path, body string) integrationPricingResponse {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:4317"
	request.Header.Set("Authorization", "Bearer "+secret)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s %s status=%d body=%s", method, path, recorder.Code, recorder.Body.String())
	}
	var response integrationPricingResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode %s %s response: %v; body=%s", method, path, err, recorder.Body.String())
	}
	return response
}
