package chstore

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestModelsDevServiceTierFormats(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, body     string
		priority, flex string
	}{
		{"v1 mode", `{"cost":{"input":5,"output":30,"cache_read":0.5},"experimental":{"modes":{"fast":{"cost":{"input":12.5,"output":75,"cache_read":1.25},"provider":{"body":{"service_tier":"priority"}}}}}}`, "0.00875", "0.0035"},
		{"v2 multipliers", `{"cost":{"text":{"input":{"price":2.5,"cache":{"read":0.25,"write":{"5m":3.125,"1h":5}}},"output":{"price":15}},"multipliers":[{"when":{"service_tier":{"equals":"priority"}},"factor":{"text":2}},{"when":{"service_tier":{"equals":"flex"}},"factor":0.5},{"when":{"region":{"equals":"us"}},"factor":100},{"when":{"service_tier":{"equals":"priority"},"region":{"equals":"us"}},"factor":100}]},"experimental":{"modes":{"fast":{"service_tier":"priority","body":{"service_tier":"priority"}}}}}`, "0.0035", "0.000875"},
		{"component factors", `{"cost":{"text":{"input":{"price":2.5,"cache":{"read":0.25}},"output":{"price":15}},"multipliers":[{"when":{"service_tier":{"equals":"priority"}},"factor":{"text":{"input":{"price":2,"cache":{"read":3}},"output":4}}},{"when":{"service_tier":{"equals":"priority"}},"factor":0.5}]}}`, "0.00325", "0.00175"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var entry modelsDevModel
			if err := json.Unmarshal([]byte(test.body), &entry); err != nil {
				t.Fatal(err)
			}
			entry.ID = "tier-model"
			original, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			rule, err := modelsDevRule("codex", entry)
			if err != nil {
				t.Fatal(err)
			}
			if err := rule.Validate(); err != nil {
				t.Fatal(err)
			}
			after, err := json.Marshal(entry)
			if err != nil || string(after) != string(original) {
				t.Fatalf("catalog conversion mutated its input: %s, err=%v", after, err)
			}
			for tier, want := range map[string]string{"priority": test.priority, "flex": test.flex} {
				event := model.Event{Provider: "codex", Model: entry.ID, ServiceTierUsed: &tier, Tokens: model.TokenUsage{Input: 100, Output: 100, Total: 200}}
				priced, err := (aggregate.PriceBook{Rules: []aggregate.PricingRule{rule}}).Price(event)
				if err != nil || priced.KnownCost == nil || priced.KnownCost.String() != want {
					t.Fatalf("tier=%s price=%+v err=%v want %s", tier, priced, err, want)
				}
			}
			if test.name == "v2 multipliers" && rule.ServiceTierPrices["priority"].CacheCreationMultiplier != "1.25" {
				t.Fatalf("wrong default cache TTL price: %+v", rule)
			}
		})
	}
}

func TestModelsDevV1FallbackPreservesTierPrices(t *testing.T) {
	t.Parallel()
	var urls []string
	client := &http.Client{Transport: modelsDevTransport(func(request *http.Request) (*http.Response, error) {
		urls = append(urls, request.URL.String())
		if request.URL.String() == modelsDevV2URL {
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"openai":{"models":{"model":{"cost":{"input":1,"output":2},"experimental":{"modes":{"fast":{"cost":{"input":2,"output":4},"provider":{"body":{"service_tier":"priority"}}}}}}}}}`))}, nil
	})}
	catalog, err := newModelsDevFetcher(client).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 2 || urls[0] != modelsDevV2URL || urls[1] != modelsDevURL || len(catalog.Rules) != 2 || catalog.Rules[0].ServiceTierPrices["priority"].InputPerMillion != 2_000_000_000 {
		t.Fatalf("urls=%v catalog=%+v", urls, catalog)
	}
}

type modelsDevTransport func(*http.Request) (*http.Response, error)

func (transport modelsDevTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestModelsDevFeedMapsProviderPricesAndPreservesUnknowns(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: modelsDevTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != modelsDevV2URL {
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestModelsDevFeedPricesKiroWithBedrockFallback(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: modelsDevTransport(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{
			"amazon-bedrock":{"models":{
				"anthropic.claude-sonnet-4-5-20250929-v1:0":{"id":"anthropic.claude-sonnet-4-5-20250929-v1:0","cost":{"input":3,"output":15,"cache_read":0.3}},
				"anthropic.claude-opus-4-6-v1":{"id":"anthropic.claude-opus-4-6-v1","cost":{"input":5.5,"output":27.5}},
				"global.anthropic.claude-opus-4-6-v1":{"id":"global.anthropic.claude-opus-4-6-v1","cost":{"input":5,"output":25}},
				"us.anthropic.claude-sonnet-4-6":{"id":"us.anthropic.claude-sonnet-4-6","cost":{"input":3.3,"output":16.5}},
				"amazon.nova-pro-v1:0":{"id":"amazon.nova-pro-v1:0","cost":{"input":0.8,"output":3.2}}
			}},
			"kiro-catalog":{"models":{"claude-opus-4-6":{"id":"claude-opus-4-6","cost":{"input":1,"output":2}}}}
		}`))}, nil
	})}
	fetch := func(bindings []CatalogBinding) map[string]model.NanoUSD {
		catalog, err := modelsDevFetcher{client: client, bindings: bindings}.Fetch(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := validateFetchedCatalog(catalog.Rules, catalog.Digest); err != nil {
			t.Fatal(err)
		}
		prices := make(map[string]model.NanoUSD)
		for _, rule := range catalog.Rules {
			if rule.Provider == "kiro" {
				prices[rule.Model] = *rule.InputPerMillion
			}
		}
		return prices
	}
	prices := fetch(nil)
	want := map[string]model.NanoUSD{"claude-sonnet-4-5-20250929": 3_000_000_000, "claude-opus-4-6": 5_000_000_000}
	if len(prices) != len(want) || prices["claude-sonnet-4-5-20250929"] != want["claude-sonnet-4-5-20250929"] || prices["claude-opus-4-6"] != want["claude-opus-4-6"] {
		t.Fatalf("kiro bedrock prices = %v, want %v", prices, want)
	}
	// An explicit Kiro catalog binding keeps precedence over the Bedrock fallback.
	prices = fetch([]CatalogBinding{{Provider: "kiro", Catalog: "kiro-catalog"}})
	if prices["claude-opus-4-6"] != 1_000_000_000 || prices["claude-sonnet-4-5-20250929"] != 3_000_000_000 {
		t.Fatalf("kiro prices with binding = %v", prices)
	}
}
