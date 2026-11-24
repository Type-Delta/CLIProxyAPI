package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagelimit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestMuseCodeRequestAdmissionAndReserveBypass(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []config.APIKeyEntry{{Key: "test-key"}, {Key: "ordinary-key"}}}}
	server := newTestServerWithConfig(t, cfg)
	manager := server.handlers.AuthManager
	manager.RegisterExecutor(executor.NewMetaExecutor(cfg))
	cred := &auth.Auth{ID: t.Name(), Provider: "meta", Status: auth.StatusActive, Attributes: map[string]string{
		"api_key": "meta-fixture", "base_url": upstream.URL + "/v1", auth.AttributeQuotaReserve: `{"weekly":30}`,
		auth.AttributeQuotaReserveBypassKeys: `["` + config.APIKeyID("test-key") + `"]`,
	}}
	live, err := manager.Register(t.Context(), cred)
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(cred.ID, "meta", []*registry.ModelInfo{{ID: "muse-wonder-full-1.0"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(cred.ID) })
	manager.ApplyQuotaReserveBars(live, []auth.QuotaReserveBar{{ID: "weekly", RemainingPercent: 20, ResetAt: time.Now().Add(time.Hour)}})
	server.usageLimitTracker.SetLimits(map[string]usagelimit.Limits{"test-key": {MaxRequests: 2}, "ordinary-key": {MaxRequests: 2}})
	request := func(method, path, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+key)
		res := httptest.NewRecorder()
		server.engine.ServeHTTP(res, req)
		return res
	}
	for _, path := range []string{"/muse-code/models", "/muse-code/config"} {
		if res := request("GET", path, "test-key"); res.Code != 200 {
			t.Fatalf("discovery %s: %d %s", path, res.Code, res.Body)
		}
	}
	if got := server.usageLimitTracker.Snapshot("test-key", time.Now()); got != nil && got.RequestsUsed != 0 {
		t.Fatalf("discovery charged: %+v", got)
	}
	for _, path := range []string{"/muse-code/search", "/muse-code/browser_open"} {
		if res := request("POST", path, "test-key"); res.Code != 200 {
			t.Fatalf("bypass action %s: %d %s", path, res.Code, res.Body)
		}
	}
	if res := request("POST", "/muse-code/search", "test-key"); res.Code != 429 {
		t.Fatalf("cap status %d: %s", res.Code, res.Body)
	}
	if got := server.usageLimitTracker.Snapshot("test-key", time.Now()); got == nil || got.RequestsUsed != 2 || got.TokensUsed != 0 {
		t.Fatalf("action accounting: %+v", got)
	}
	if res := request("POST", "/muse-code/search", "ordinary-key"); res.Code == 200 {
		t.Fatal("ordinary key bypassed reserve")
	}
}
