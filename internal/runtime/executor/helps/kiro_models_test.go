package helps

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestKiroModelCatalogPaginationAndIsolation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Query().Get("origin") != "AI_EDITOR" {
			t.Errorf("catalog request = %s %s", r.Method, r.URL)
		}
		limit := 800000
		if r.Header.Get("Authorization") == "Bearer second" {
			limit = 320000
		}
		if r.URL.Query().Get("profileArn") == "other-profile" {
			limit = 600000
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("nextToken") == "" {
			_, _ = fmt.Fprintf(w, `{"models":[{"modelId":"first","tokenLimits":{"maxInputTokens":%d,"maxOutputTokens":1000}}],"nextToken":"page-2"}`, limit)
			return
		}
		if r.URL.Query().Get("nextToken") != "page-2" {
			t.Errorf("pagination token = %q", r.URL.Query().Get("nextToken"))
		}
		_, _ = fmt.Fprint(w, `{"models":[{"modelId":"second","tokenLimits":{"maxInputTokens":200000,"maxOutputTokens":2000}}]}`)
	}))
	t.Cleanup(server.Close)
	request, errRequest := http.NewRequest(http.MethodGet, server.URL+"/ListAvailableModels?origin=AI_EDITOR&profileArn=profile", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	request.Header.Set("Authorization", "Bearer first")
	var catalog KiroModelCatalog
	lookup := func(credential, model string) KiroModelLimits {
		return catalog.Lookup(context.Background(), server.Client(), credential, request, model)
	}
	if got := lookup("one", "second"); got.MaxInputTokens != 200000 || got.MaxOutputTokens != 2000 {
		t.Fatalf("second-page model = %+v", got)
	}
	if got := lookup("one", "first"); got.MaxInputTokens != 800000 {
		t.Fatalf("first-page model = %+v", got)
	}
	if got := lookup("one", "missing"); got != (KiroModelLimits{}) {
		t.Fatalf("unknown model = %+v, want unknown limits", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("cached catalog calls = %d, want 2 pages", calls.Load())
	}
	request.Header.Set("Authorization", "Bearer second")
	if got := lookup("two", "first"); got.MaxInputTokens != 320000 {
		t.Fatalf("credential reused another account's limits: %+v", got)
	}
	request.URL.RawQuery = "origin=AI_EDITOR&profileArn=other-profile"
	if got := lookup("two", "first"); got.MaxInputTokens != 600000 {
		t.Fatalf("profile reused another profile's limits: %+v", got)
	}
	if calls.Load() != 6 {
		t.Fatalf("isolated catalog calls = %d, want 6", calls.Load())
	}
}

func TestKiroModelCatalogRefreshRetainsLastSuccess(t *testing.T) {
	var calls, phase atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch phase.Load() {
		case 0:
			_, _ = fmt.Fprint(w, `{"models":[{"modelId":"old","tokenLimits":{"maxInputTokens":800000}}]}`)
		case 1:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case 2:
			_, _ = fmt.Fprint(w, `{"models":[{"modelId":"new","tokenLimits":{"maxInputTokens":320000}}]}`)
		}
	}))
	t.Cleanup(server.Close)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	catalog := KiroModelCatalog{nowFunc: func() time.Time { return now }}
	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	lookup := func(model string) KiroModelLimits {
		return catalog.Lookup(context.Background(), server.Client(), "account", request, model)
	}
	if got := lookup("old"); got.MaxInputTokens != 800000 {
		t.Fatal(got)
	}
	phase.Store(1)
	now = now.Add(registry.ModelsRefreshInterval)
	if got := lookup("old"); got.MaxInputTokens != 800000 {
		t.Fatalf("failed refresh discarded last good limits: %+v", got)
	}
	lookup("old")
	if calls.Load() != 2 {
		t.Fatalf("failed refresh was not throttled: calls=%d", calls.Load())
	}
	phase.Store(2)
	now = now.Add(time.Minute)
	if got := lookup("new"); got.MaxInputTokens != 320000 {
		t.Fatalf("refreshed limits = %+v", got)
	}
	if got := lookup("old"); got.MaxInputTokens != 0 {
		t.Fatalf("removed model retained stale limits after successful refresh: %+v", got)
	}
}

func TestKiroModelCatalogSharesRefreshAndCancelsWaiters(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		_, _ = fmt.Fprint(w, `{"models":[{"modelId":"model","tokenLimits":{"maxInputTokens":700000}}]}`)
	}))
	t.Cleanup(server.Close)
	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	var catalog KiroModelCatalog
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if got := catalog.Lookup(context.Background(), server.Client(), "account", request, "model"); got.MaxInputTokens != 700000 {
				t.Errorf("shared refresh result = %+v", got)
			}
		})
	}
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := catalog.Lookup(ctx, server.Client(), "account", request, "model"); got.MaxInputTokens != 0 {
		t.Fatalf("canceled cache miss = %+v", got)
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent refreshes = %d, want 1", calls.Load())
	}
}

func TestKiroModelCatalogRejectsIncompleteDiscovery(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `{"models":null}`, `{"models":[`,
		`{"models":[],"nextToken":"repeated"}`,
		`{"models":[{"modelId":"model","tokenLimits":{"maxInputTokens":-1}}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, body)
			}))
			t.Cleanup(server.Close)
			request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
			var catalog KiroModelCatalog
			if got := catalog.Lookup(context.Background(), server.Client(), "account", request, "model"); got != (KiroModelLimits{}) {
				t.Fatalf("invalid catalog produced limits: %+v", got)
			}
		})
	}
}
