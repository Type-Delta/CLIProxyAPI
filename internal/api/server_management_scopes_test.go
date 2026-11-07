package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"golang.org/x/crypto/bcrypt"
)

func TestManagementScopedKeysOnlyStartupAndRouteSecurity(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	hash, err := bcrypt.GenerateFromPassword([]byte("viewer-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{RemoteManagement: config.RemoteManagement{Keys: []config.ManagementKey{{ID: "viewer", Name: "Viewer", SecretKey: string(hash), Scope: config.ManagementScopeReadOnly}}}}
	s := newTestServerWithConfig(t, cfg)
	if !s.managementRoutesEnabled.Load() || !s.managementRoutesRegistered.Load() {
		t.Fatal("keys-only management disabled")
	}
	safeGET := map[string]bool{}
	for _, path := range []string{"/overview", "/server/latest-version", "/observability/logs", "/observability/logs/errors", "/observability/logs/errors/:name", "/observability/logs/requests/:id", "/observability/usage/api-keys", "/session", "/capabilities", "/storage", "/credentials", "/credentials/models", "/routing/model-definitions/:channel", "/telemetry/health", "/telemetry/schema", "/analytics/health", "/analytics/summary", "/analytics/timeseries", "/analytics/dimensions", "/analytics/events", "/analytics/events/:attempt_id", "/analytics/attempts/:attempt_id", "/analytics/pricing", "/analytics/pricing/catalog-providers", "/analytics/providers", "/analytics/quotas", "/analytics/quota-history", "/analytics/keys", "/analytics/leaderboard", "/analytics/query", "/plugins/:id/quota"} {
		safeGET["/v8/management"+path] = true
	}
	safePOST := map[string]bool{}
	for _, path := range []string{"/analytics/query", "/analytics/exports", "/telemetry/query", "/requests/api-call", "/plugins/:id/quota"} {
		safePOST["/v8/management"+path] = true
	}
	denied := 0
	for _, route := range s.engine.Routes() {
		if !strings.HasPrefix(route.Path, "/v8/management/") && !strings.HasPrefix(route.Path, "/v0/management/") {
			continue
		}
		if route.Path == "/v8/management/oauth/callback" || route.Path == "/v0/management/oauth-callback" {
			continue
		}
		if route.Method == "GET" && safeGET[route.Path] || route.Method == "POST" && safePOST[route.Path] {
			continue
		}
		t.Run(route.Method+" "+route.Path, func(t *testing.T) {
			path := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(route.Path, ":id", "test"), ":name", "test"), "*path", "management/keys")
			req := httptest.NewRequest(route.Method, path, strings.NewReader(`{}`))
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set("Authorization", "Bearer viewer-secret")
			req.Header.Set("Upgrade", "websocket")
			res := httptest.NewRecorder()
			s.engine.ServeHTTP(res, req)
			if res.Code != 403 {
				t.Fatalf("unsafe route escaped allowlist: %d %s", res.Code, res.Body)
			}
		})
		denied++
	}
	if denied < 100 {
		t.Fatalf("too few registered mutation/secret routes covered: %d", denied)
	}
	req := httptest.NewRequest("GET", "/v8/management/session", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer viewer-secret")
	res := httptest.NewRecorder()
	s.engine.ServeHTTP(res, req)
	if res.Code != 200 || res.Body.String() != `{"scope":"read-only"}` {
		t.Fatal("session failed", res.Code, res.Body)
	}
}

func TestManagementScopedKeyAvailabilityReload(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	s := newTestServerWithConfig(t, &config.Config{})
	if s.managementRoutesEnabled.Load() {
		t.Fatal("management unexpectedly enabled")
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("new-viewer"), bcrypt.MinCost)
	next := s.getConfig().CloneForRuntime()
	next.RemoteManagement.Keys = []config.ManagementKey{{ID: "viewer", Name: "Viewer", SecretKey: string(hash), Scope: config.ManagementScopeReadOnly}}
	s.UpdateClients(next)
	if !s.managementRoutesEnabled.Load() || !s.managementRoutesRegistered.Load() {
		t.Fatal("adding scoped key did not enable routes")
	}
	req := httptest.NewRequest("GET", "/v8/management/session", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer new-viewer")
	res := httptest.NewRecorder()
	s.engine.ServeHTTP(res, req)
	if res.Code != 200 {
		t.Fatal("hot-reloaded key rejected", res.Code, res.Body)
	}
	next = next.CloneForRuntime()
	next.RemoteManagement.Keys = nil
	s.UpdateClients(next)
	res = httptest.NewRecorder()
	s.engine.ServeHTTP(res, req)
	if res.Code != 404 {
		t.Fatal("removing last key did not disable routes", res.Code, res.Body)
	}
}
