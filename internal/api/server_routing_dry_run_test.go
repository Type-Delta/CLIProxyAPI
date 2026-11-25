package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type dryRunTestExecutor struct {
	auth.ProviderExecutor
	calls []string
}

func (*dryRunTestExecutor) Identifier() string { return "claude" }

func (e *dryRunTestExecutor) Execute(_ context.Context, credential *auth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls = append(e.calls, credential.ID)
	return coreexecutor.Response{Payload: []byte(`{"object":"chat.completion","choices":[{"message":{"role":"assistant","content":"OK"}}]}`)}, nil
}

const dryRunTestModel = "claude-opus-5-5"

func TestRoutingDryRunResultMatchesInference(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	cfg := &config.Config{SDKConfig: config.SDKConfig{
		APIKeys: []config.APIKeyEntry{{Key: "test-key", Label: "regular"}, {Key: "bypass-key", Label: "bypass"}},
	}}
	server := newTestServerWithConfig(t, cfg)
	manager := server.handlers.AuthManager
	exec := &dryRunTestExecutor{}
	manager.RegisterExecutor(exec)
	manager.SetRetryConfig(0, 0, 0)

	const quotaID, reserveID = "dry-run-result-quota", "dry-run-result-reserve"
	now := time.Now()
	quota := &auth.Auth{
		ID: quotaID, Provider: "claude", Label: "Quota fixture", Status: auth.StatusActive, Unavailable: true,
		NextRetryAfter: now.Add(2 * time.Hour), LastError: &auth.Error{Message: "quota exhausted", HTTPStatus: 429},
		Quota: auth.QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(2 * time.Hour)},
	}
	reserve := &auth.Auth{
		ID: reserveID, Provider: "claude", Label: "Reserve fixture", Status: auth.StatusActive,
		Attributes: map[string]string{
			auth.AttributeQuotaReserve:           `{"weekly":30}`,
			auth.AttributeQuotaReserveBypassKeys: `["` + config.APIKeyID("bypass-key") + `"]`,
		},
	}
	reg := registry.GetGlobalRegistry()
	for _, credential := range []*auth.Auth{quota, reserve} {
		reg.RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{{ID: dryRunTestModel}})
		t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
		if _, err := manager.Register(t.Context(), credential); err != nil {
			t.Fatal(err)
		}
	}
	stored, _ := manager.GetByID(reserveID)
	manager.ApplyQuotaReserveBars(stored, []auth.QuotaReserveBar{{ID: "weekly", RemainingPercent: 20, ResetAt: now.Add(time.Hour)}})

	for _, test := range []struct {
		name, model, key, label, errorClass string
		status                              int
	}{
		{name: "reserve releases before quota", model: dryRunTestModel, key: "test-key", label: "regular", status: 429, errorClass: "model_cooldown"},
		{name: "reserve bypass", model: dryRunTestModel, key: "bypass-key", label: "bypass", status: 200},
		{name: "unknown model", model: "unknown-dry-run-result-model", key: "test-key", label: "regular", status: 400, errorClass: "model_not_found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := func(path, token, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				server.engine.ServeHTTP(response, req)
				return response
			}
			before := len(exec.calls)
			dry := request("/v8/management/routing/dry-run", "test-management-key",
				`{"model":"`+test.model+`","api_key_label":"`+test.label+`"}`)
			var payload struct {
				Result handlers.RoutingDryRunResult `json:"result"`
			}
			if dry.Code != http.StatusOK {
				t.Fatalf("dry run HTTP %d: %s", dry.Code, dry.Body)
			}
			if err := json.Unmarshal(dry.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			result := payload.Result
			if len(exec.calls) != before || result.Body == nil || result.Status == nil || *result.Status != test.status ||
				result.Success == nil || *result.Success != (test.status == 200) {
				t.Fatalf("unexpected dry run: %s", dry.Body)
			}

			live := request("/v1/chat/completions", test.key,
				`{"model":"`+test.model+`","messages":[{"role":"user","content":"Reply OK"}]}`)
			if live.Code != *result.Status {
				t.Fatalf("live HTTP %d: %s, dry run: %s", live.Code, live.Body, dry.Body)
			}
			if test.status == 200 {
				if *result.Body != "success" || result.ErrorClass != nil || result.RetryAfter != nil ||
					result.Credential == nil || result.Credential.Label != "Reserve fixture" ||
					len(exec.calls) != before+1 || result.Credential.ID != exec.calls[before] {
					t.Fatalf("dry run did not predict the credential that served the request: %s, attempts %v", dry.Body, exec.calls)
				}
				return
			}
			if result.Credential != nil || result.ErrorClass == nil || *result.ErrorClass != test.errorClass ||
				len(exec.calls) != before {
				t.Fatalf("dry run and live error differ: dry %s, live %s", dry.Body, live.Body)
			}
			if test.status == 429 {
				if result.RetryAfter == nil || *result.RetryAfter < 3597 || *result.RetryAfter > 3600 ||
					ojson.Get(*result.Body, "error.reset_seconds").Int() != *result.RetryAfter ||
					ojson.Get(*result.Body, "error.last_upstream_error").String() != "quota exhausted" {
					t.Fatalf("dry run did not retain the cooldown deadline and cause: %s, live headers %v", dry.Body, live.Header())
				}
				liveSeconds, err := strconv.ParseInt(live.Header().Get("Retry-After"), 10, 64)
				if err != nil || liveSeconds > *result.RetryAfter || *result.RetryAfter-liveSeconds > 2 {
					t.Fatalf("dry run Retry-After %d, live %q", *result.RetryAfter, live.Header().Get("Retry-After"))
				}
				for _, field := range []string{"error.code", "error.message", "error.model", "error.provider", "error.last_upstream_error"} {
					if ojson.Get(*result.Body, field).String() != ojson.Get(live.Body.String(), field).String() {
						t.Fatalf("%s differs: dry %s, live %s", field, dry.Body, live.Body)
					}
				}
			} else if result.RetryAfter != nil || *result.Body != live.Body.String() {
				t.Fatalf("dry run and live error differ: dry %s, live %s", dry.Body, live.Body)
			}
		})
	}
}
