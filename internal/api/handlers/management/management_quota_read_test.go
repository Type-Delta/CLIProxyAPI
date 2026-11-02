package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestReadOnlyPluginQuotaCredentialIsolationAndErrors(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			auth := &coreauth.Auth{ID: "plugin-reader", Provider: "codex", Metadata: map[string]any{"access_token": "plugin-fixture-secret"}}
			index := auth.EnsureIndex()
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			host := pluginhost.New()
			calls := 0
			host.RegisterPluginForTest("selected", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{QuotaProvider: &mockQuotaProvider{
				identifier: "codex",
				fetchFn: func(_ context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
					calls++
					return pluginapi.QuotaFetchResponse{}, errors.New("upstream rejected " + req.Metadata["access_token"].(string))
				},
			}}})
			host.RegisterPluginForTest("unrelated", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{QuotaProvider: &mockQuotaProvider{
				identifier: "other-provider",
				fetchFn: func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
					t.Fatal("unrelated plugin received credential")
					return pluginapi.QuotaFetchResponse{}, nil
				},
			}}})
			h, router := scopedManagementFixture(t)
			h.SetAuthManager(manager)
			h.SetPluginHost(host)
			router.GET("/v8/management/plugins/:id/quota", h.Middleware(), h.GetPluginQuota)
			router.POST("/v8/management/plugins/:id/quota", h.Middleware(), h.FetchPluginQuota)
			for _, pluginID := range []string{"unrelated", "selected"} {
				path := "/v8/management/plugins/" + pluginID + "/quota"
				body := `{"auth_index":"` + index + `"}`
				if method == http.MethodGet {
					path += "?auth_index=" + index
				}
				res := scopedRequest(router, method, path, "viewer-secret", body)
				want := http.StatusBadGateway
				if pluginID == "unrelated" {
					want = http.StatusForbidden
				}
				if res.Code != want || strings.Contains(res.Body.String(), "plugin-fixture-secret") {
					t.Fatalf("%s: %d %s", pluginID, res.Code, res.Body)
				}
			}
			if calls != 1 {
				t.Fatalf("selected plugin calls = %d, want 1", calls)
			}
		})
	}
}

func TestReadOnlyQuotaValidation(t *testing.T) {
	auth := &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"account_id": "acct"}}
	base := apiCallRequest{Method: "GET", URL: codexUsageQuotaURL, Header: map[string]string{"Authorization": "Bearer $TOKEN$", "Chatgpt-Account-Id": "acct"}}
	u, _ := url.Parse(base.URL)
	if !authorizeReadOnlyQuota(&base, auth, u) {
		t.Fatal("panel quota rejected")
	}
	tests := []struct {
		name   string
		change func(*apiCallRequest)
	}{
		{"arbitrary host", func(b *apiCallRequest) { b.URL = "https://example.invalid/echo" }},
		{"userinfo", func(b *apiCallRequest) { b.URL = "https://attacker:pass@chatgpt.com/backend-api/wham/usage" }},
		{"http", func(b *apiCallRequest) { b.URL = "http://chatgpt.com/backend-api/wham/usage" }},
		{"port", func(b *apiCallRequest) { b.URL = "https://chatgpt.com:444/backend-api/wham/usage" }},
		{"query", func(b *apiCallRequest) { b.URL += "?redirect=https://example.invalid" }},
		{"encoded path", func(b *apiCallRequest) { b.URL = "https://chatgpt.com/backend-api/wham/%75sage" }},
		{"fragment", func(b *apiCallRequest) { b.URL += "#fragment" }},
		{"write", func(b *apiCallRequest) { b.Method = "POST" }},
		{"body", func(b *apiCallRequest) { b.Data = "$TOKEN$" }},
		{"proxy", func(b *apiCallRequest) { b.ProxyURL = "https://example.invalid" }},
		{"host header", func(b *apiCallRequest) { b.Header = map[string]string{"Host": "example.invalid"} }},
		{"token header", func(b *apiCallRequest) { b.Header = map[string]string{"X-Steal": "$TOKEN$"} }},
		{"cookie", func(b *apiCallRequest) { b.Header = map[string]string{"Cookie": "foo=bar"} }},
		{"account", func(b *apiCallRequest) { b.Header = map[string]string{"Chatgpt-Account-Id": "other"} }},
		{"user-agent substitution", func(b *apiCallRequest) { b.Header = map[string]string{"User-Agent": "$TOKEN$"} }},
		{"reset credits", func(b *apiCallRequest) { b.Method = "POST"; b.URL = codexConsumeURL; b.Data = `{"amount":1}` }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := base
			tc.change(&b)
			target, _ := url.Parse(b.URL)
			if authorizeReadOnlyQuota(&b, auth, target) {
				t.Fatal("unsafe request allowed")
			}
		})
	}
	for _, provider := range []string{"claude", "kimi", "antigravity", "xai", "devin", "unknown"} {
		if authorizeReadOnlyQuota(&base, &coreauth.Auth{Provider: provider}, u) {
			t.Fatalf("credential confused: %s", provider)
		}
	}
	ag := &coreauth.Auth{Provider: "antigravity", Metadata: map[string]any{"project_id": "project"}}
	b := apiCallRequest{Method: "POST", URL: "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", Data: `{"project":"project"}`}
	target, _ := url.Parse(b.URL)
	if !authorizeReadOnlyQuota(&b, ag, target) {
		t.Fatal("safe Antigravity quota denied")
	}
	for _, data := range []string{`{"project":"other"}`, `{"project":"project","token":"$TOKEN$"}`, `{"project":{}}`, `$TOKEN$`} {
		b.Data = data
		if authorizeReadOnlyQuota(&b, ag, target) {
			t.Fatal("unsafe quota RPC body allowed")
		}
	}
	xai := &coreauth.Auth{Provider: "xai"}
	b = apiCallRequest{Method: "POST", URL: xaiHealthURL, Data: `{"model":"grok-4.5","messages":[{"role":"user","content":"ping"}],"max_tokens":1,"stream":false}`}
	target, _ = url.Parse(b.URL)
	if authorizeReadOnlyQuota(&b, xai, target) {
		t.Fatal("paid health probe allowed")
	}
}

func readOnlyQuotaCall(t *testing.T, h *Handler, body apiCallRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(res)
	c.Request = httptest.NewRequest("POST", "/v8/management/requests/api-call", strings.NewReader(string(raw)))
	c.Set(managementScopeContextKey, config.ManagementScopeReadOnly)
	h.APICall(c)
	return res
}

func TestReadOnlyQuotaActualTransportAndSecretRedaction(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	proxy.setResponse(200, http.Header{"Set-Cookie": []string{"session=usable-secret"}, "Authorization": []string{"Bearer test-token"}}, `{"remaining":42,"api_key":"usable-secret","access_token":"test-token","nested":{"value":"prefix test-token suffix"}}`)
	body := apiCallRequest{AuthIndexCamel: ptrString("credential-a-codex"), Method: "GET", URL: codexUsageQuotaURL, Header: map[string]string{"Authorization": "Bearer $TOKEN$"}}
	res := readOnlyQuotaCall(t, h, body)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `remaining`) || strings.Contains(res.Body.String(), "usable-secret") || strings.Contains(res.Body.String(), "test-token") || strings.Contains(res.Body.String(), "Set-Cookie") {
		t.Fatal(res.Code, res.Body)
	}
	if proxy.count.Load() != 1 {
		t.Fatal("quota request did not reach real transport")
	}
	body.Header = map[string]string{"Host": "example.invalid", "Authorization": "Bearer $TOKEN$"}
	if res = readOnlyQuotaCall(t, h, body); res.Code != 403 || proxy.count.Load() != 1 {
		t.Fatal("denied call reached transport", res.Code, res.Body)
	}
}

func TestReadOnlyQuotaRedirectCannotExfiltrate(t *testing.T) {
	var received atomic.Int64
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1) }))
	defer sink.Close()
	proxy := newQuotaProxy(t)
	proxy.setResponse(302, http.Header{"Location": []string{sink.URL}}, `{}`)
	h := quotaHandler(proxy)
	res := readOnlyQuotaCall(t, h, apiCallRequest{AuthIndexCamel: ptrString("credential-a-codex"), Method: "GET", URL: codexUsageQuotaURL, Header: map[string]string{"Authorization": "Bearer $TOKEN$"}})
	if res.Code != 200 || received.Load() != 0 || !strings.Contains(res.Body.String(), `"status_code":302`) {
		t.Fatal("redirect followed", res.Code, res.Body, received.Load())
	}
}

func TestReadOnlyCredentialListCannotExposeAPIKey(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{ID: "credential", Index: "index", Provider: "codex", Attributes: map[string]string{"api_key": "usable-api-secret", "auth_kind": "api_key", "runtime_only": "true", "usage_probe": "zai"}, Metadata: map[string]any{"quota_reserve_bypass_keys": []string{"another-secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{}, "", manager)
	c, res := v2Request("GET", "/v8/management/credentials", "")
	c.Set(managementScopeContextKey, config.ManagementScopeReadOnly)
	c.Set(ConfigV8ContextKey, true)
	h.ListAuthFiles(c)
	if res.Code != 200 || strings.Contains(res.Body.String(), "usable-api-secret") || strings.Contains(res.Body.String(), "another-secret") {
		t.Fatal(res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), `"files":[{`) {
		t.Fatal("credential not listed")
	}
}

func ptrString(value string) *string { return &value }

func TestReadOnlyQuotaPreservesAccountingButStripsTokenCredentials(t *testing.T) {
	var payload any
	if err := json.Unmarshal([]byte(`{"input_tokens":120,"outputTokens":42,"remaining_tokens":900,"tokens_limit":1000,"access_token":12345,"refreshToken":"secret","tokens":"disguised-secret","token_limit":"secret","nested":{"api_key":"secret","input_tokens":3,"echo":"prefix secret suffix"}}`), &payload); err != nil {
		t.Fatal(err)
	}
	got := sanitizeQuotaJSON(payload, []string{"secret"}).(map[string]any)
	for key, want := range map[string]float64{"input_tokens": 120, "outputTokens": 42, "remaining_tokens": 900, "tokens_limit": 1000} {
		if got[key] != want {
			t.Fatalf("accounting %s = %v, want %v", key, got[key], want)
		}
	}
	for _, key := range []string{"access_token", "refreshToken", "tokens", "token_limit"} {
		if _, ok := got[key]; ok {
			t.Fatalf("credential field survived: %s", key)
		}
	}
	nested := got["nested"].(map[string]any)
	if nested["input_tokens"] != float64(3) || nested["echo"] != "prefix [redacted] suffix" {
		t.Fatal(nested)
	}
	if _, ok := nested["api_key"]; ok {
		t.Fatal("nested credential survived")
	}
}
