package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	for _, path := range []string{"/observability/requests/in-flight", "/live-requests", "/live-requests/stream", "/overview", "/server/latest-version", "/observability/logs", "/observability/logs/errors", "/observability/logs/errors/:name", "/observability/logs/requests/:id", "/observability/usage/api-keys", "/session", "/capabilities", "/storage", "/credentials", "/credentials/models", "/routing/model-definitions/:channel", "/telemetry/health", "/telemetry/schema", "/analytics/health", "/analytics/summary", "/analytics/timeseries", "/analytics/dimensions", "/analytics/events", "/analytics/events/:attempt_id", "/analytics/attempts/:attempt_id", "/analytics/pricing", "/analytics/pricing/catalog-providers", "/analytics/providers", "/analytics/quotas", "/analytics/quota-history", "/analytics/keys", "/analytics/leaderboard", "/analytics/query", "/plugins/:id/quota"} {
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
	readLive := httptest.NewRequest("GET", "/v8/management/live-requests", nil)
	readLive.RemoteAddr = "127.0.0.1:1234"
	readLive.Header.Set("Authorization", "Bearer viewer-secret")
	liveResponse := httptest.NewRecorder()
	s.engine.ServeHTTP(liveResponse, readLive)
	if liveResponse.Code != 200 || !strings.Contains(liveResponse.Body.String(), `"requests":`) {
		t.Fatalf("read-only live requests: %d %s", liveResponse.Code, liveResponse.Body)
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

func TestLiveRequestsStreamRoute(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	hash, err := bcrypt.GenerateFromPassword([]byte("stream-viewer"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{RemoteManagement: config.RemoteManagement{Keys: []config.ManagementKey{{ID: "viewer", SecretKey: string(hash), Scope: config.ManagementScopeReadOnly}}}}
	s := newTestServerWithConfig(t, cfg)
	server := httptest.NewServer(s.engine)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/v8/management/live-requests/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer stream-viewer")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	for name, want := range map[string]string{"Content-Type": "text/event-stream", "Cache-Control": "no-store", "X-Accel-Buffering": "no"} {
		if got := resp.Header.Get(name); got != want {
			t.Fatalf("%s=%q", name, got)
		}
	}
	body := bufio.NewReader(resp.Body)
	line, err := body.ReadString('\n')
	if err != nil || resp.StatusCode != 200 || !strings.HasPrefix(line, "data: ") || !strings.Contains(line, `"full":true`) || !strings.Contains(line, `"requests":[`) {
		t.Fatalf("%d %s %v", resp.StatusCode, line, err)
	}
	// Revoking the key ends the open stream before it sends anything more.
	revoked := *cfg
	revoked.RemoteManagement.Keys = nil
	revoked.RemoteManagement.SecretKey = "unrelated-hash"
	s.mgmt.SetConfig(&revoked)
	rest, err := io.ReadAll(body)
	if err != nil || strings.Contains(string(rest), "data: ") {
		t.Fatalf("revoked stream kept sending: %q %v", rest, err)
	}
	cancel()
	denied := httptest.NewRecorder()
	unauthenticated := httptest.NewRequest("GET", "/v8/management/live-requests/stream", nil)
	unauthenticated.RemoteAddr = "127.0.0.1:1234"
	s.engine.ServeHTTP(denied, unauthenticated)
	if denied.Code != 401 {
		t.Fatalf("unauthenticated stream: %d", denied.Code)
	}
}

func TestLiveRequestsStreamEndsWhenManagementStopsServing(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	hash, err := bcrypt.GenerateFromPassword([]byte("stream-viewer"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{RemoteManagement: config.RemoteManagement{Keys: []config.ManagementKey{{ID: "viewer", SecretKey: string(hash), Scope: config.ManagementScopeReadOnly}}}}
	s := newTestServerWithConfig(t, cfg)
	server := httptest.NewServer(s.engine)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/v8/management/live-requests/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer stream-viewer")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := bufio.NewReader(resp.Body)
	if line, errRead := body.ReadString('\n'); errRead != nil || !strings.Contains(line, `"full":true`) {
		t.Fatalf("%s %v", line, errRead)
	}
	// Home mode (or losing the management secret) turns the Management API off; an open stream
	// must end too, as new requests already get 404.
	s.managementRoutesEnabled.Store(false)
	rest, err := io.ReadAll(body)
	if err != nil || strings.Contains(string(rest), "data: ") {
		t.Fatalf("stream kept sending after management stopped: %q %v", rest, err)
	}
}
