package api

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexAlphaSearchReserveBypass(t *testing.T) {
	server := newTestServer(t)
	server.handlers.AuthManager.RegisterExecutor(executor.NewCodexAutoExecutor(server.getConfig()))
	credential := &auth.Auth{ID: "audit-dl038-search", Provider: "codex", Status: auth.StatusActive,
		Metadata:   map[string]any{"type": "codex", "access_token": "audit-oauth-token"},
		Attributes: map[string]string{auth.AttributeQuotaReserve: `{"weekly":30}`, auth.AttributeQuotaReserveBypassKeys: `["` + config.APIKeyID("test-key") + `"]`},
	}
	registry.GetGlobalRegistry().RegisterClient(credential.ID, "codex", []*registry.ModelInfo{{ID: "gpt-6.1-sol"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credential.ID) })
	if _, err := server.handlers.AuthManager.Register(t.Context(), credential); err != nil {
		t.Fatal(err)
	}
	calls := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", codexSearchSIWCTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"results":[]}`)), Request: req}, nil
	}))
	request := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(`{"model":"gpt-6.1-sol","query":"test"}`)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer test-key")
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, req)
		t.Logf("status=%d upstream_calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
		return recorder.Code
	}
	if code := request(); code != 200 {
		t.Fatalf("baseline status=%d", code)
	}
	live, _ := server.handlers.AuthManager.GetByID(credential.ID)
	server.handlers.AuthManager.ApplyQuotaReserveBars(live, []auth.QuotaReserveBar{{ID: "weekly", RemainingPercent: 20, ResetAt: time.Now().Add(time.Hour)}})
	if code := request(); code != 200 {
		t.Fatalf("bypass key incorrectly withheld from search: got=%d want=200", code)
	}
}
