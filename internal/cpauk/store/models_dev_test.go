package store

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

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
