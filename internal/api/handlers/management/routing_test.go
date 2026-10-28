package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
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
	if !body.HomeMode || len(body.Credentials) != 2 {
		t.Fatalf("home_mode=%t credentials=%+v", body.HomeMode, body.Credentials)
	}
	if body.Credentials[0].ID != "oauth-file" || body.Credentials[0].Kind != "oauth" || body.Credentials[0].Weight != 1 {
		t.Fatalf("oauth credential = %+v", body.Credentials[0])
	}
	if body.Credentials[1].ID != "api-key-auth" || body.Credentials[1].Kind != "api-key" || !body.Credentials[1].Disabled || body.Credentials[1].Priority != 7 || body.Credentials[1].Weight != 3 || body.Credentials[1].Prefix != "team/" {
		t.Fatalf("API-key credential = %+v", body.Credentials[1])
	}
	if len(body.ClientTypes) != 7 || body.ClientTypes[0] != "claude" || body.ClientTypes[6] != "generic" {
		t.Fatalf("client types = %v", body.ClientTypes)
	}
	for _, secret := range []string{"oauth-access-secret", "oauth-token-secret", "upstream-api-key-secret", `"attributes"`, `"metadata"`} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("routing credentials response exposed %q: %s", secret, response.Body.String())
		}
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
