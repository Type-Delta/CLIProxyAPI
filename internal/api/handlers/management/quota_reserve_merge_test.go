package management

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestClaudePartialQuotaReadPreservesFableReserve(t *testing.T) {
	fixture := newGSRProbeFixture(t)
	h, auth := gsrProbeHandler(t, "claude", map[string]string{coreauth.AttributeQuotaReserve: `{"seven-day-fable":10}`})
	h.authManager.RegisterExecutor(&refreshRecordExecutor{provider: "claude"})
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "claude-fable-5-1"}, {ID: "claude-opus-5"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	reset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	fixture.setResponse(http.StatusOK, fmt.Sprintf(`{"limits":[{"kind":"weekly_scoped","percent":95,"is_active":true,"resets_at":%q,"scope":{"model":{"display_name":"Fable"}}}]}`, reset))
	quotaCall(t, h, nil, auth.Index, http.MethodGet, claudeUsageQuotaURL, "", "")
	execute := func(model string) error {
		_, err := h.authManager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		return err
	}
	if err := execute("claude-fable-5-1"); err == nil {
		t.Fatal("Fable should be held at its reserve")
	}
	fixture.setResponse(http.StatusOK, `{"five_hour":{"utilization":20},"seven_day":{"utilization":20}}`)
	quotaCall(t, h, nil, auth.Index, http.MethodGet, claudeResetGrantsQuotaURL, "", "")
	if err := execute("claude-fable-5-1"); err == nil {
		t.Fatal("partial grant read discarded Fable reserve")
	}
	if err := execute("claude-opus-5"); err != nil {
		t.Fatalf("Fable reserve blocked Opus: %v", err)
	}
}
