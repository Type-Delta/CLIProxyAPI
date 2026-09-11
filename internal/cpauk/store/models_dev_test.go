package store

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

type modelsDevTransport func(*http.Request) (*http.Response, error)

func (transport modelsDevTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestModelsDevFeedMapsProviderPricesAndPreservesUnknowns(t *testing.T) {
	client := &http.Client{Transport: modelsDevTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != modelsDevURL {
			t.Fatalf("unexpected catalog URL %s", request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{
			"openai":{"models":{
				"gpt-test":{"id":"gpt-test","cost":{"input":2.5,"output":1e1,"cache_read":0.625}},
				"unknown":{"id":"unknown","cost":{"input":null,"output":5}}
			}},
			"unmapped":{"models":{"gpt-test":{"id":"gpt-test","cost":{"input":99,"output":99}}}}
		}`))}, nil
	})}
	catalog, err := newModelsDevFetcher(client).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Rules) != 4 || !model.IsFullKeyID(catalog.Digest) {
		t.Fatalf("unexpected catalog size or digest: %d %q", len(catalog.Rules), catalog.Digest)
	}
	for _, rule := range catalog.Rules {
		if rule.Provider != "openai" && rule.Provider != "codex" {
			t.Fatalf("unexpected provider %q", rule.Provider)
		}
		if rule.Model == "unknown" {
			if rule.InputPerMillion != nil || rule.OutputPerMillion != nil {
				t.Fatal("missing input price became known")
			}
			continue
		}
		if *rule.InputPerMillion != 2_500_000_000 || *rule.OutputPerMillion != 10_000_000_000 || rule.CacheReadMultiplier != "0.25" {
			t.Fatalf("incorrect decimal prices or cache rate: %+v", rule)
		}
	}
}

func TestModelsDevPriceRejectsNegativeAndOverflow(t *testing.T) {
	for _, value := range []string{"-0.0000000001", "1e30"} {
		if _, _, err := parseModelsDevPrice([]byte(value)); err == nil {
			t.Fatalf("accepted invalid price %s", value)
		}
	}
	price, known, err := parseModelsDevPrice([]byte("0"))
	if err != nil || !known || price != 0 {
		t.Fatalf("free price = %d, %v, %v", price, known, err)
	}
}

func TestModelsDevFeedAppliesCatalogBindingsAndListsProviders(t *testing.T) {
	client := &http.Client{Transport: modelsDevTransport(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{
			"zai-coding-plan":{"name":"Z.AI Coding Plan","models":{"glm":{"id":"glm","cost":{"input":1,"output":2}}}},
			"groq":{"models":{"llama":{"id":"llama","cost":{"input":3,"output":4}}}},
			"unknown":{"name":"Unknown","models":{"model":{"id":"model","cost":{"input":5,"output":6}}}}
		}`))}, nil
	})}
	fetcher := modelsDevFetcher{client: client, bindings: []CatalogBinding{
		{Provider: "openai-compatible-zai", Catalog: "zai-coding-plan"},
		{Provider: "openai-compatible-groq"},
		{Provider: "openai-compatible-missing", Catalog: "not-in-feed"},
	}}
	catalog, err := fetcher.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Rules) != 2 {
		t.Fatalf("binding rules=%d, want 2: %+v", len(catalog.Rules), catalog.Rules)
	}
	seen := make(map[string]bool, len(catalog.Rules))
	for _, rule := range catalog.Rules {
		seen[rule.Provider+":"+rule.Model] = true
	}
	if !seen["openai-compatible-zai:glm"] || !seen["openai-compatible-groq:llama"] {
		t.Fatalf("binding rules=%v", seen)
	}
	if len(catalog.Providers) != 3 || catalog.Providers[0] != (CatalogProvider{ID: "groq", Name: "groq"}) || catalog.Providers[2] != (CatalogProvider{ID: "zai-coding-plan", Name: "Z.AI Coding Plan"}) {
		t.Fatalf("providers=%+v", catalog.Providers)
	}
}

func TestPricingCatalogPersistsProviders(t *testing.T) {
	fetcher := &testPricingFetcher{catalog: PricingCatalog{
		Rules: testPricingCatalog(1, 2).Rules, Digest: strings.Repeat("b", 64),
		Providers: []CatalogProvider{{ID: "zai", Name: "Z.AI"}, {ID: "openai", Name: "OpenAI"}},
	}}
	database := openDiscoveryStore(t, filepath.Join(t.TempDir(), "analytics.db"), fetcher, refTime(time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)))
	if _, err := database.RefreshPricing(context.Background()); err != nil {
		t.Fatal(err)
	}
	providers, updatedAt, err := database.CatalogProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 2 || updatedAt.IsZero() {
		t.Fatalf("providers=%+v updated_at=%v", providers, updatedAt)
	}
}

func refTime(now time.Time) *time.Time { return &now }
