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

// Replay the three reads made by CPAMC's Codex quota data layer. The reset
// details add headers that usage and subscription reads do not send.
func TestReadOnlyCodexPanelRequestsActualTransport(t *testing.T) {
	for _, variant := range []string{"production", "id-token-only", "id-token-precedence", "metadata-snake", "metadata-camel", "attribute-snake", "attribute-camel", "legacy-account"} {
		t.Run(variant, func(t *testing.T) {
			proxy := newQuotaProxy(t)
			h := quotaHandler(proxy)
			metadata := map[string]any{"access_token": "test-token"}
			attributes := map[string]string{}
			switch variant {
			case "production":
				metadata["account_id"] = "acc-oauth-test"
				metadata["id_token"] = makeOAuthTestJWT("pro")
			case "id-token-only":
				metadata["id_token"] = makeOAuthTestJWT("pro")
			case "id-token-precedence":
				metadata["id_token"] = makeOAuthTestJWT("pro")
				metadata["account_id"] = "stale-account"
			case "metadata-snake":
				metadata["chatgpt_account_id"] = "acc-oauth-test"
			case "metadata-camel":
				metadata["chatgptAccountId"] = "acc-oauth-test"
			case "attribute-snake":
				attributes["chatgpt_account_id"] = "acc-oauth-test"
			case "attribute-camel":
				attributes["chatgptAccountId"] = "acc-oauth-test"
			case "legacy-account":
				metadata["account_id"] = "acc-oauth-test"
			}
			auth := &coreauth.Auth{ID: "panel-codex", Index: "panel-codex", Provider: "codex", Metadata: metadata, Attributes: attributes}
			if _, err := h.authManager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			headers := map[string]string{
				"Authorization":      "Bearer $TOKEN$",
				"Content-Type":       "application/json",
				"User-Agent":         "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)",
				"Chatgpt-Account-Id": "acc-oauth-test",
			}
			for _, target := range []string{codexUsageQuotaURL, "https://chatgpt.com/backend-api/subscriptions?account_id=acc-oauth-test", codexResetCreditsURL} {
				if target == codexResetCreditsURL {
					headers["Accept"] = "application/json"
					headers["OpenAI-Beta"] = "codex-1"
					headers["Originator"] = "Codex Desktop"
					proxy.setResponse(200, nil, `{"rate_limit_reset_credits":[{"expires_at":"2027-01-01T00:00:00Z"}]}`)
				} else {
					proxy.setResponse(200, nil, `{"plan_type":"pro","active_until":"2027-01-01T00:00:00Z"}`)
				}
				body := apiCallRequest{AuthIndexCamel: ptrString(auth.Index), Method: "GET", URL: target, Header: headers}
				res := readOnlyQuotaCall(t, h, body)
				if res.Code != 200 || !strings.Contains(res.Body.String(), `"status_code":200`) {
					t.Fatalf("%s: HTTP %d %s", target, res.Code, res.Body)
				}
				if target == codexResetCreditsURL && !strings.Contains(res.Body.String(), "expires_at") {
					t.Fatalf("manual reset expiry missing: %s", res.Body)
				}
			}
			if proxy.count.Load() != 3 {
				t.Fatalf("transport calls = %d, want all three panel reads", proxy.count.Load())
			}
			for _, tc := range []struct {
				name   string
				change func(*apiCallRequest)
			}{
				{"account header mismatch", func(b *apiCallRequest) { b.Header["Chatgpt-Account-Id"] = "other" }},
				{"subscription account mismatch", func(b *apiCallRequest) { b.URL = "https://chatgpt.com/backend-api/subscriptions?account_id=other" }},
				{"subscription extra query", func(b *apiCallRequest) {
					b.URL = "https://chatgpt.com/backend-api/subscriptions?account_id=acc-oauth-test&redirect=other"
				}},
				{"subscription duplicate account", func(b *apiCallRequest) {
					b.URL = "https://chatgpt.com/backend-api/subscriptions?account_id=acc-oauth-test&account_id=other"
				}},
				{"reset query", func(b *apiCallRequest) { b.URL += "?account_id=acc-oauth-test" }},
				{"beta mismatch", func(b *apiCallRequest) { b.Header["OpenAI-Beta"] = "other" }},
				{"originator mismatch", func(b *apiCallRequest) { b.Header["Originator"] = "other" }},
				{"beta credential escape", func(b *apiCallRequest) { b.Header["OpenAI-Beta"] = "$TOKEN$" }},
				{"originator credential escape", func(b *apiCallRequest) { b.Header["Originator"] = "$TOKEN$" }},
				{"beta on usage", func(b *apiCallRequest) { b.URL = codexUsageQuotaURL }},
				{"reset consumption", func(b *apiCallRequest) { b.Method = "POST"; b.URL = codexConsumeURL; b.Data = `{"amount":1}` }},
				{"reset write", func(b *apiCallRequest) { b.Method = "POST" }},
				{"arbitrary URL", func(b *apiCallRequest) { b.URL = "https://example.invalid/echo" }},
				{"host header", func(b *apiCallRequest) { b.Header["Host"] = "example.invalid" }},
				{"literal credential", func(b *apiCallRequest) { b.Header["Authorization"] = "Bearer test-token" }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					requestHeaders := make(map[string]string, len(headers))
					for key, value := range headers {
						requestHeaders[key] = value
					}
					body := apiCallRequest{AuthIndexCamel: ptrString(auth.Index), Method: "GET", URL: codexResetCreditsURL, Header: requestHeaders}
					tc.change(&body)
					if strings.HasPrefix(body.URL, "https://chatgpt.com/backend-api/subscriptions") {
						delete(body.Header, "OpenAI-Beta")
						delete(body.Header, "Originator")
					}
					res := readOnlyQuotaCall(t, h, body)
					if res.Code != http.StatusForbidden || proxy.count.Load() != 3 {
						t.Fatalf("unsafe request reached transport: HTTP %d %s, calls %d", res.Code, res.Body, proxy.count.Load())
					}
				})
			}
		})
	}
}

func TestReadOnlyCodexListedAccountOverridesStaleAliases(t *testing.T) {
	for _, variant := range []string{"metadata-snake", "metadata-camel", "attribute-snake", "attribute-camel"} {
		t.Run(variant, func(t *testing.T) {
			proxy := newQuotaProxy(t)
			h, router := scopedManagementFixture(t)
			h.cfg.AuthDir = t.TempDir()
			h.SetAuthManager(coreauth.NewManager(nil, nil, nil))
			metadata := map[string]any{"access_token": "test-token", "account_id": "acc-oauth-test", "id_token": makeOAuthTestJWT("pro")}
			attributes := map[string]string{"runtime_only": "true"}
			switch variant {
			case "metadata-snake":
				metadata["chatgpt_account_id"] = "stale-account"
			case "metadata-camel":
				metadata["chatgptAccountId"] = "stale-account"
			case "attribute-snake":
				attributes["chatgpt_account_id"] = "stale-account"
			case "attribute-camel":
				attributes["chatgptAccountId"] = "stale-account"
			}
			auth := &coreauth.Auth{ID: "listing-codex", Index: "listing-codex", Provider: "codex", Metadata: metadata, Attributes: attributes}
			if _, err := h.authManager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			router.GET("/v8/management/credentials", h.Middleware(), h.ListAuthFiles)
			router.POST("/v8/management/requests/api-call", h.Middleware(), h.APICall)
			listed := scopedRequest(router, "GET", "/v8/management/credentials", "viewer-secret", "")
			var listing struct {
				Files []struct {
					AuthIndex string `json:"auth_index"`
					IDToken   struct {
						AccountID string `json:"chatgpt_account_id"`
					} `json:"id_token"`
				} `json:"files"`
			}
			if listed.Code != 200 || json.Unmarshal(listed.Body.Bytes(), &listing) != nil || len(listing.Files) != 1 || listing.Files[0].IDToken.AccountID != "acc-oauth-test" {
				t.Fatal("credential listing", listed.Code, listed.Body)
			}
			file := listing.Files[0]
			for _, target := range []string{codexUsageQuotaURL, "https://chatgpt.com/backend-api/subscriptions?account_id=" + url.QueryEscape(file.IDToken.AccountID), codexResetCreditsURL} {
				headers := map[string]string{"Authorization": "Bearer $TOKEN$", "Chatgpt-Account-Id": file.IDToken.AccountID}
				if target == codexResetCreditsURL {
					headers["OpenAI-Beta"] = "codex-1"
					headers["Originator"] = "Codex Desktop"
				}
				proxy.setResponse(200, nil, `{"available_count":1,"credits":[{"expires_at":"2027-01-01T00:00:00Z"}]}`)
				body, err := json.Marshal(apiCallRequest{AuthIndexCamel: &file.AuthIndex, Method: "GET", URL: target, Header: headers})
				if err != nil {
					t.Fatal(err)
				}
				res := scopedRequest(router, "POST", "/v8/management/requests/api-call", "viewer-secret", string(body))
				if res.Code != 200 || !strings.Contains(res.Body.String(), `"status_code":200`) {
					t.Fatalf("%s: %d %s", target, res.Code, res.Body)
				}
			}
			if proxy.count.Load() != 3 {
				t.Fatalf("transport calls = %d, want 3", proxy.count.Load())
			}
			body, err := json.Marshal(apiCallRequest{AuthIndexCamel: &file.AuthIndex, Method: "GET", URL: codexUsageQuotaURL, Header: map[string]string{"Authorization": "Bearer $TOKEN$", "Chatgpt-Account-Id": "stale-account"}})
			if err != nil {
				t.Fatal(err)
			}
			res := scopedRequest(router, "POST", "/v8/management/requests/api-call", "viewer-secret", string(body))
			if res.Code != http.StatusForbidden || proxy.count.Load() != 3 {
				t.Fatal("stale account reached upstream", res.Code, res.Body)
			}
		})
	}
}
