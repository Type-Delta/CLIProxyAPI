package management

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"gopkg.in/yaml.v3"
)

func TestReadOnlyManagementObservations(t *testing.T) {
	h, r := scopedManagementFixture(t)
	h.cfg.Debug = true
	h.cfg.LoggingToFile = true
	h.cfg.RequestRetry = 3
	h.cfg.ProxyURL = "http://proxy-user:proxy-pass@proxy.example:8080/private?token=proxy-query#secret"
	h.cfg.CodexKey = []config.CodexKey{{APIKey: "provider-secret"}}
	var plugin config.PluginInstanceConfig
	if err := yaml.Unmarshal([]byte("api-key: plugin-hidden-secret\nheaders: {X-Credential: plugin-header-secret}\n"), &plugin); err != nil {
		t.Fatal(err)
	}
	h.cfg.Plugins.Configs = map[string]config.PluginInstanceConfig{"fixture": plugin}
	h.logDir = t.TempDir()
	h.authManager = coreauth.NewManager(nil, nil, nil)
	for _, id := range []string{"first", "duplicate"} {
		_, err := h.authManager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "codex", Attributes: map[string]string{"api_key": "usage-secret", "base_url": "https://private.example"}, Success: 2, Metadata: map[string]any{"access_token": "runtime-secret", "cookie": "runtime-cookie"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	v8 := r.Group("/v8/management", h.Middleware())
	v8.GET("/overview", h.GetManagementOverview)
	v8.GET("/observability/usage/api-keys", h.GetAPIKeyUsage)
	v8.GET("/observability/logs", h.GetLogs)
	v8.GET("/observability/logs/errors", h.GetRequestErrorLogs)
	v8.GET("/observability/logs/errors/:name", h.DownloadRequestErrorLog)
	v8.GET("/observability/logs/requests/:id", h.GetRequestLogByID)
	v8.DELETE("/observability/logs", h.DeleteLogs)
	v8.POST("/models/refresh", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	v8.GET("/server/latest-version", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	raw := "=== REQUEST INFO ===\nVersion: test\nURL: https://user:pass@example.test/v1?token=query-secret\nMethod: POST\n=== HEADERS ===\nAuthorization: Bearer unknown-secret\nCookie: unknown-cookie\n=== REQUEST BODY ===\n{\"arbitrary\":\"body-secret\"}\n=== API REQUEST ===\nHeaders:\nX-Custom: unknown-upstream-secret\nBody:\nupstream-secret\n=== RESPONSE ===\nStatus: 401\nSet-Cookie: response-secret\nresponse-body-secret\n"
	if err := os.WriteFile(filepath.Join(h.logDir, "error-test-abc.log"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	app := "plugin-hidden-secret plugin-header-secret\nruntime-secret runtime-cookie\nCookie: unknown-cookie\n{\"cookie\":\"json-cookie\",\"api_key\":\"json-api-secret\"}\nhttps://app.example?key=unknown-query\n[2026-10-01 10:00:00] useful diagnostic provider-secret usage-secret Authorization: Bearer unknown-secret password=unknown-password https://proxy-user:proxy-pass@proxy.example?token=proxy-query\n"
	if err := os.WriteFile(filepath.Join(h.logDir, "main.log"), []byte(app), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/overview", "/observability/usage/api-keys", "/observability/logs", "/observability/logs/errors", "/observability/logs/errors/error-test-abc.log", "/observability/logs/requests/abc", "/server/latest-version"} {
		res := scopedRequest(r, "GET", "/v8/management"+path, "viewer-secret", "")
		if res.Code != 200 {
			t.Fatalf("%s: %d %s", path, res.Code, res.Body)
		}
		for _, secret := range []string{"plugin-hidden-secret", "plugin-header-secret", "provider-secret", "usage-secret", "private.example", "proxy-user", "proxy-pass", "proxy-query", "unknown-secret", "unknown-cookie", "body-secret", "upstream-secret", "response-secret", "response-body-secret", "query-secret", "unknown-password", "runtime-secret", "runtime-cookie", "json-cookie", "json-api-secret", "unknown-query"} {
			if strings.Contains(res.Body.String(), secret) {
				t.Fatalf("%s leaked %s: %s", path, secret, res.Body)
			}
		}
		if path == "/overview" {
			var overview struct {
				Config            map[string]any
				Counts            map[string]int
				ProviderKeyCounts map[string]int
			}
			if err := json.Unmarshal(res.Body.Bytes(), &overview); err != nil {
				t.Fatal(err)
			}
			if len(overview.Config) != 7 || overview.Config["proxyUrl"] != "http://proxy.example:8080" || overview.Counts["providerKeys"] != 1 || overview.ProviderKeyCounts["codex"] != 1 || len(overview.ProviderKeyCounts) != 8 {
				t.Fatal(res.Body)
			}
		}
		if path == "/observability/usage/api-keys" {
			var usage map[string]map[string]apiKeyUsageEntry
			if err := json.Unmarshal(res.Body.Bytes(), &usage); err != nil {
				t.Fatal(err)
			}
			if len(usage["codex"]) != 1 || usage["codex"]["key-1"].Success != 4 || len(usage["codex"]["key-1"].RecentRequests) != 20 {
				t.Fatal(res.Body)
			}
		}
		if strings.Contains(path, "/errors/error-") || strings.Contains(path, "/requests/abc") {
			if !strings.Contains(res.Body.String(), "Status: 401") || !strings.Contains(res.Body.String(), "Method: POST") || !strings.Contains(res.Header().Get("Content-Disposition"), "error-test-abc.log") {
				t.Fatal(res.Body, res.Header())
			}
		}
	}
	for _, tc := range []struct{ method, path string }{{"DELETE", "/observability/logs"}, {"POST", "/models/refresh"}, {"GET", "/config"}, {"PUT", "/config/observability/request-log"}} {
		if res := scopedRequest(r, tc.method, "/v8/management"+tc.path, "viewer-secret", "{}"); res.Code != 403 {
			t.Fatal(tc, res.Code, res.Body)
		}
	}
	for _, path := range []string{"/observability/logs/errors/not-a-log", "/observability/logs/errors/error-..%5coutside.log", "/observability/logs/requests/..%5coutside"} {
		res := scopedRequest(r, "GET", "/v8/management"+path, "viewer-secret", "")
		if res.Code != 400 && res.Code != 404 {
			t.Fatal(path, res.Code, res.Body)
		}
	}
	for _, path := range []string{"/observability/logs/errors/error-test-abc.log", "/observability/logs/requests/abc"} {
		res := scopedRequest(r, "GET", "/v8/management"+path, "owner-secret", "")
		if res.Code != 200 || res.Body.String() != raw {
			t.Fatal("full access changed", res.Code, res.Body)
		}
	}
	res := scopedRequest(r, "GET", "/v8/management/observability/usage/api-keys", "owner-secret", "")
	if !strings.Contains(res.Body.String(), "https://private.example|usage-secret") {
		t.Fatal("full usage changed", res.Body)
	}
}

func TestReadOnlyRequestLogCannotReenterMetadataFromPayload(t *testing.T) {
	h, router := scopedManagementFixture(t)
	h.logDir = t.TempDir()
	router.GET("/v8/management/observability/logs/errors/:name", h.Middleware(), h.DownloadRequestErrorLog)
	request := &record.Request{
		Method: "POST", URL: "https://upstream.example/v1/responses", Status: 200,
		Body:     []byte("=== REQUEST INFO ===\nMethod: private-request-marker\n"),
		Response: []byte("=== REQUEST INFO ===\nMethod: private-response-marker\n"),
	}
	raw := logging.RenderRequestLog(request)
	if err := os.WriteFile(filepath.Join(h.logDir, "error-spoof.log"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	path := "/v8/management/observability/logs/errors/error-spoof.log"
	res := scopedRequest(router, "GET", path, "viewer-secret", "")
	if res.Code != 200 || !strings.Contains(res.Body.String(), "Method: POST") || strings.Contains(res.Body.String(), "private-") {
		t.Fatal(res.Code, res.Body)
	}
	res = scopedRequest(router, "GET", path, "owner-secret", "")
	if res.Code != 200 || res.Body.String() != string(raw) {
		t.Fatal("full-access request log changed", res.Code, res.Body)
	}
}
