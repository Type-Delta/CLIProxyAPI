package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestProviderQuotaRecoveryPreservesIndependentAuthForce(t *testing.T) {
	for _, reset := range []bool{false, true} {
		for _, quotaModel := range []string{"", "force-recovery-model"} {
			name := "fetch/" + quotaModel
			if reset {
				name = "reset/" + quotaModel
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				m := NewManager(nil, nil, nil)
				m.RegisterExecutor(&refreshMockExecutor{id: "claude"})
				auth, err := m.Register(ctx, &Auth{ID: t.Name(), Provider: "claude", Status: StatusActive})
				if err != nil {
					t.Fatal(err)
				}
				const model = "force-recovery-model"
				registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				forceRetry, quotaRetry := 3*time.Hour, time.Hour
				m.MarkResult(ctx, Result{AuthID: auth.ID, RetryAfter: &forceRetry, Error: &Error{Code: ErrorCodeForceCooldown, HTTPStatus: 503, Message: "independent force"}})
				m.MarkResult(ctx, Result{AuthID: auth.ID, Model: quotaModel, RetryAfter: &quotaRetry, Error: &Error{HTTPStatus: 429, Message: "quota exhausted"}})
				before, _ := m.GetByID(auth.ID)
				deadline := effectiveAuthForceUntil(before)
				if !deadline.After(time.Now()) || before.LastError == nil || before.LastError.HTTPStatus != 429 {
					t.Fatal("fixture did not retain independent force beneath the newer quota error")
				}
				if reset {
					_, err = m.ApplyProviderQuotaReset(ctx, auth.ID, []string{model}, nil)
				} else {
					_, err = m.ApplyProviderQuotaRecovery(ctx, auth.ID, []string{model})
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, _, errPick := m.pickNext(ctx, "claude", model, cliproxyexecutor.Options{}, nil); errPick == nil {
					t.Fatal("quota recovery bypassed the independent auth force deadline")
				}
				after, _ := m.GetByID(auth.ID)
				if !effectiveAuthForceUntil(after).Equal(deadline) {
					t.Fatalf("force deadline changed: got %v want %v", effectiveAuthForceUntil(after), deadline)
				}
				if blocked, _, _ := isAuthBlockedForModel(after, model, deadline.Add(time.Second)); blocked {
					t.Fatal("recovered quota still blocks after the independent force expires")
				}
			})
		}
	}
}
