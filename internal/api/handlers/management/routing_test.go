package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
)

func TestGetRoutingCredentialsIsSecretFreeAndComplete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfigSnapshot(&config.Config{Home: config.HomeConfig{Enabled: true}})
	for _, auth := range []*coreauth.Auth{
		{
			ID:       "oauth-file",
			Provider: "claude",
			Label:    "person@example.com",
			Status:   coreauth.StatusActive,
			Metadata: map[string]any{"access_token": "oauth-access-secret"},
		},
		{
			ID:            "aaa-error",
			Provider:      "codex",
			Label:         "failing account",
			Status:        coreauth.StatusError,
			StatusMessage: "refresh-failure-private-detail",
		},
		{
			ID:       "api-key-auth",
			Provider: "codex",
			Label:    "team account",
			Prefix:   "team/",
			Status:   coreauth.StatusDisabled,
			Disabled: true,
			Attributes: map[string]string{
				coreauth.AttributeAuthKind: coreauth.AuthKindAPIKey,
				coreauth.AttributeAPIKey:   "upstream-api-key-secret",
				"priority":                 "7",
				coreauth.AttributeWeight:   "3",
			},
			Metadata: map[string]any{"access_token": "oauth-token-secret"},
		},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	handler := &Handler{authManager: manager, cfg: &config.Config{}}
	request := httptest.NewRequest(http.MethodGet, "/v8/management/routing/credentials", nil)
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = request
	handler.GetRoutingCredentials(ctx)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body routingCredentialsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.HomeMode || len(body.Credentials) != 3 {
		t.Fatalf("home_mode=%t credentials=%+v", body.HomeMode, body.Credentials)
	}
	if body.Credentials[0].ID != "oauth-file" || body.Credentials[0].Kind != "oauth" || body.Credentials[0].Weight != 1 || body.Credentials[0].Status != string(coreauth.StatusActive) {
		t.Fatalf("oauth credential = %+v", body.Credentials[0])
	}
	if body.Credentials[1].ID != "aaa-error" || body.Credentials[1].Status != string(coreauth.StatusError) || !strings.Contains(response.Body.String(), `"status":"error"`) || !strings.Contains(response.Body.String(), `"status":"active"`) {
		t.Fatalf("error credential = %+v, body=%s", body.Credentials[1], response.Body.String())
	}
	if body.Credentials[2].ID != "api-key-auth" || body.Credentials[2].Kind != "api-key" || !body.Credentials[2].Disabled || body.Credentials[2].Priority != 7 || body.Credentials[2].Weight != 3 || body.Credentials[2].Prefix != "team/" || body.Credentials[2].Status != string(coreauth.StatusDisabled) {
		t.Fatalf("API-key credential = %+v", body.Credentials[2])
	}
	if len(body.ClientTypes) != 7 || body.ClientTypes[0] != "claude" || body.ClientTypes[6] != "generic" {
		t.Fatalf("client types = %v", body.ClientTypes)
	}
	for _, secret := range []string{"refresh-failure-private-detail", `"status_message"`, `"statusMessage"`, "oauth-access-secret", "oauth-token-secret", "upstream-api-key-secret", `"attributes"`, `"metadata"`} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("routing credentials response exposed %q: %s", secret, response.Body.String())
		}
	}
}

func TestGetRoutingCredentialsV8GroupNames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, family := range []string{"gemini", "interactions", "codex", "xai", "meta", "claude", "vertex", "openai-compatibility"} {
		t.Run(family, func(t *testing.T) {
			raw := fmt.Sprintf(`config-version: 8
api-keys:
  %s:
    - name: fake-high
      base-url: https://api.example.invalid
      keys:
        - api-key: dummy-named-secret
          label: routing-label
    - base-url: https://api.example.invalid
      keys:
        - api-key: dummy-unnamed-secret
          label: unnamed-label
`, family)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
				Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(auths) != 2 {
				t.Fatalf("got %d auths, want 2", len(auths))
			}
			manager := coreauth.NewManager(nil, nil, nil)
			for _, auth := range auths {
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
			}
			handler := &Handler{authManager: manager, cfg: cfg}
			router := gin.New()
			router.GET("/v8/management/routing/credentials", handler.GetRoutingCredentials)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management/routing/credentials", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "dummy-") {
				t.Fatal("response leaked API key")
			}
			var body struct {
				Credentials []map[string]json.RawMessage `json:"credentials"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Credentials) != 2 {
				t.Fatalf("credentials=%s", response.Body.String())
			}
			seen := map[string]bool{}
			for _, credential := range body.Credentials {
				var label string
				if err := json.Unmarshal(credential["label"], &label); err != nil {
					t.Fatal(err)
				}
				seen[label] = true
				switch label {
				case "routing-label":
					var name string
					if err := json.Unmarshal(credential["name"], &name); err != nil || name != "fake-high" {
						t.Fatalf("name=%q, err=%v", name, err)
					}
				case "unnamed-label":
					if _, exists := credential["name"]; exists {
						t.Fatal("unnamed credential included name")
					}
				default:
					t.Fatalf("label changed to %q", label)
				}
			}
			if !seen["routing-label"] || !seen["unnamed-label"] {
				t.Fatalf("labels=%v", seen)
			}
		})
	}
}

func TestPostRoutingDryRunRejectsUnknownAPIKeyLabel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	handler := &Handler{
		authManager: manager,
		cfg: &config.Config{SDKConfig: config.SDKConfig{
			APIKeys: []config.APIKeyEntry{{Key: "client-secret", Label: "known"}},
		}},
	}
	request := httptest.NewRequest(http.MethodPost, "/v8/management/routing/dry-run", strings.NewReader(`{"model":"gpt-5","api_key_label":"unknown"}`))
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = request
	handler.PostRoutingDryRun(ctx)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"path":"api_key_label"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPostRoutingDryRunValidatesDraftRulePaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	handler := &Handler{
		authManager: manager,
		cfg: &config.Config{SDKConfig: config.SDKConfig{
			APIKeys: []config.APIKeyEntry{{Key: "client-secret", Label: "known"}},
		}},
	}
	body := `{"model":"gpt-5","api_key_label":"known","rules":[{"when":{"all":[{"model":{"regex":"["}}]},"route-to":["auth-a"]}]}`
	request := httptest.NewRequest(http.MethodPost, "/v8/management/routing/dry-run", strings.NewReader(body))
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = request
	handler.PostRoutingDryRun(ctx)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"path":"routing.rules[0].when.all[0].model.regex"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPostRoutingDryRunWarnsForUnknownDraftAndLiveTargets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, draft := range []bool{true, false} {
		name := "live rules"
		if draft {
			name = "draft rules"
		}
		t.Run(name, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			cfg := &config.Config{Routing: config.RoutingConfig{
				Rules: []config.RoutingRule{{Name: "first", RouteTo: []string{"auth-known"}}, {Name: "second", RouteTo: []string{"zai-x"}}},
			}}
			if !draft {
				manager.SetConfigSnapshot(cfg)
			}
			handler := &Handler{authManager: manager, cfg: cfg}
			body := `{"model":"gpt-5"}`
			if draft {
				body = `{"model":"gpt-5","rules":[{"name":"first","route-to":["auth-known"]},{"name":"second","route-to":["zai-x"]}]}`
			}
			request := httptest.NewRequest(http.MethodPost, "/v8/management/routing/dry-run", strings.NewReader(body))
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Request = request
			handler.PostRoutingDryRun(ctx)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var result struct {
				Warnings []string `json:"warnings"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Warnings) != 2 || result.Warnings[1] != `routing.rules[1].route-to[0]: unknown credential "zai-x"` {
				t.Fatalf("warnings=%v", result.Warnings)
			}
		})
	}
}

func TestPostRoutingDryRunReturnsTraceAndCandidateLabels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	provider, model, authID := "routing-handler-test", "routing-handler-model", "routing-handler-auth"
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.RegisterExecutor(&refreshRecordExecutor{provider: provider})
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
		ID:       authID,
		Provider: provider,
		Label:    "Primary account",
		Status:   coreauth.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	handler := &Handler{
		authManager: manager,
		cfg: &config.Config{SDKConfig: config.SDKConfig{
			APIKeys: []config.APIKeyEntry{{Key: "client-secret", Label: "research"}},
		}},
	}
	body := `{"model":"routing-handler-model","api_key_label":"RESEARCH","client_type":"codex","headers":{"X-Team":"infra"},"session_id":"dry-run-session","rules":[{"name":"draft","when":{"all":[{"model":{"is":"routing-handler-model"}},{"api-key":{"is":"research"}},{"header":{"name":"x-team","is":"infra"}},{"client":{"is":"codex"}}]},"route-to":["routing-handler-auth"]},{"name":"unknown-label","when":{"api-key":{"is":"missing"}},"route-to":["routing-handler-auth"]}]}`
	request := httptest.NewRequest(http.MethodPost, "/v8/management/routing/dry-run", strings.NewReader(body))
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = request
	handler.PostRoutingDryRun(ctx)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Trace    routing.Trace `json:"trace"`
		Warnings []string      `json:"warnings"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Trace.Candidates) != 1 || result.Trace.Candidates[0].ID != authID || result.Trace.Candidates[0].Label != "Primary account" {
		t.Fatalf("trace candidates = %+v", result.Trace.Candidates)
	}
	if result.Trace.Rules == nil || result.Trace.Rules.Applied != 0 || result.Trace.Pick < 0 || len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], `unknown API-key label "missing"`) {
		t.Fatalf("trace=%+v warnings=%v", result.Trace, result.Warnings)
	}
}

func TestGetRoutingCredentialsIncludesAPIKeyCooldowns(t *testing.T) {
	for _, home := range []bool{false, true} {
		t.Run(fmt.Sprint("home=", home), func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetConfigSnapshot(&config.Config{Home: config.HomeConfig{Enabled: home}})
			retryAt := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
			for _, auth := range []*coreauth.Auth{
				{ID: "config-api-key", Provider: "openai-compatibility", Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindAPIKey}, ModelStates: map[string]*coreauth.ModelState{"m": {Unavailable: true, NextRetryAfter: retryAt, LastError: &coreauth.Error{HTTPStatus: 403}}}},
				{ID: "disabled-api-key", Provider: "openai-compatibility", Disabled: true, Status: coreauth.StatusDisabled},
			} {
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
			}
			h := &Handler{authManager: manager, cfg: &config.Config{}}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/routing/credentials", nil)
			h.GetRoutingCredentials(c)
			var result struct {
				ObservedAt  time.Time `json:"observed_at"`
				Credentials []struct {
					ID        string          `json:"id"`
					Cooldowns json.RawMessage `json:"cooldowns"`
				} `json:"credentials"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.ObservedAt.IsZero() || len(result.Credentials) != 2 {
				t.Fatalf("missing observation or credentials: %s", w.Body)
			}
			for _, credential := range result.Credentials {
				if len(credential.Cooldowns) == 0 {
					t.Fatalf("missing cooldowns field: %s", w.Body)
				}
				if home {
					if string(credential.Cooldowns) != "null" {
						t.Fatalf("Home cooldowns must be unknown: %s", credential.Cooldowns)
					}
					continue
				}
				var cooldowns []coreauth.CooldownView
				if err := json.Unmarshal(credential.Cooldowns, &cooldowns); err != nil {
					t.Fatal(err)
				}
				if credential.ID == "config-api-key" {
					if len(cooldowns) != 1 || cooldowns[0].ModelKey != "m" || !cooldowns[0].RetryAt.Equal(retryAt) || cooldowns[0].HTTPStatus != 403 || cooldowns[0].RemainingSeconds <= 0 {
						t.Fatalf("API-key cooldown missing: %s", w.Body)
					}
				} else if string(credential.Cooldowns) != "[]" {
					t.Fatalf("disabled credential without timer must have []: %s", credential.Cooldowns)
				}
			}
		})
	}
}

func TestPostRoutingDryRunRequiresModel(t *testing.T) {
	for _, body := range []string{`{}`, `{"model":""}`, `{"model":"  \t\n"}`} {
		t.Run(body, func(t *testing.T) {
			h := &Handler{authManager: coreauth.NewManager(nil, nil, nil), cfg: &config.Config{}}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v8/management/routing/dry-run", strings.NewReader(body))
			h.PostRoutingDryRun(c)
			if w.Code != http.StatusBadRequest || w.Body.String() != `{"error":"model is required","path":"model"}` {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
		})
	}
}
