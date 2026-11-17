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

func TestQuotaRecoveryAfterTransientCapacityHTTP(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, reset := range []bool{false, true} {
			for _, capacityRetry := range []time.Duration{0, 15 * time.Minute, 5 * time.Hour} {
				t.Run(fmt.Sprintf("%s/reset=%t/capacity=%s", provider, reset, capacityRetry), func(t *testing.T) {
					ctx := context.Background()
					var h *Handler
					var auth *coreauth.Auth
					var reconcile func()
					modelID := "gpt-5.5"
					if provider == "claude" {
						// The selected grant explicitly refunds Fable's allowance.
						f := newClaudeResetFixture(t, `["seven_day_overage_included"]`)
						h, auth, modelID = f.handler, f.auth, "claude-fable-5-2"
						if reset {
							reconcile = func() { f.claim(t) }
						} else {
							f.usageCode = http.StatusOK
							f.usage = `{"limits":[{"kind":"weekly_scoped","percent":20,"is_active":true,"scope":{"model":{"display_name":"Fable"}}}]}`
							reconcile = func() { quotaCall(t, h, nil, auth.Index, http.MethodGet, claudeUsageQuotaURL, "", "", true) }
						}
					} else {
						fixture := newGSRProbeFixture(t)
						h, auth = gsrProbeHandler(t, provider, nil)
						if reset {
							fixture.setResponse(http.StatusOK, `{}`)
							reconcile = func() { quotaCall(t, h, nil, auth.Index, http.MethodPost, codexConsumeURL, "", `{}`) }
						} else {
							fixture.setResponse(http.StatusOK, `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":10}}}`)
							reconcile = func() { quotaCall(t, h, nil, auth.Index, http.MethodGet, codexUsageQuotaURL, "", "", true) }
						}
						h.authManager.RegisterExecutor(&refreshRecordExecutor{provider: provider})
						registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: modelID}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					}
					retry := 3 * time.Hour
					h.authManager.MarkResult(ctx, coreauth.Result{AuthID: auth.ID, Model: modelID, RetryAfter: &retry, Error: &coreauth.Error{HTTPStatus: 429, Message: "quota exhausted"}})
					beforeCapacity := time.Now()
					h.authManager.MarkResult(ctx, coreauth.Result{AuthID: auth.ID, Model: modelID, RetryAfter: &capacityRetry, Error: &coreauth.Error{Code: coreauth.ErrorCodeTransientRateLimit, HTTPStatus: 429, Message: "capacity"}})
					afterCapacity := time.Now()
					reconcile()
					got, _ := h.authManager.GetByID(auth.ID)
					state := got.ModelStates[modelID]
					if state.Quota.Exceeded || !state.Quota.NextRecoverAt.IsZero() {
						t.Errorf("recovered quota still exhausted: %+v", state.Quota)
					}
					if capacityRetry > 0 && (state.NextRetryAfter.Before(beforeCapacity.Add(capacityRetry)) || state.NextRetryAfter.After(afterCapacity.Add(capacityRetry))) {
						t.Errorf("capacity deadline=%v, want independent %s retry", state.NextRetryAfter, capacityRetry)
					}
					reg := registry.GetGlobalRegistry()
					if suspended := reg.IsModelSuspendedForClient(auth.ID, modelID); suspended != (capacityRetry > 0) {
						t.Errorf("registry suspended=%t after recovery, capacity=%s", suspended, capacityRetry)
					}
					_, errExecute := h.authManager.Execute(ctx, []string{provider}, cliproxyexecutor.Request{Model: modelID}, cliproxyexecutor.Options{})
					if (errExecute != nil) != (capacityRetry > 0) {
						t.Errorf("Execute after recovery, capacity=%s: %v", capacityRetry, errExecute)
					}
				})
			}
		}
	}
}
