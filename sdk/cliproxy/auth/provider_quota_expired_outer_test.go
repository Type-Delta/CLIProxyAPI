package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestSharedQuotaExpiryPreservesIndependentAuthLifetime(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, legacyOuter := range []bool{false, true} {
			for _, delayedRegistration := range []bool{false, true} {
				for _, elapsed := range []time.Duration{0, 2 * time.Hour} {
					t.Run(fmt.Sprintf("%s/legacy=%t/delayed=%t/elapsed=%s", provider, legacyOuter, delayedRegistration, elapsed), func(t *testing.T) {
						ctx := context.Background()
						const model = "quota-expired-outer"
						newManager := func(register bool) *Manager {
							m := NewManager(nil, nil, nil)
							m.RegisterExecutor(&refreshMockExecutor{id: provider})
							if register {
								if _, err := m.Register(ctx, &Auth{ID: t.Name(), Provider: provider}); err != nil {
									t.Fatal(err)
								}
							}
							return m
						}
						m := newManager(true)
						registry.GetGlobalRegistry().RegisterClient(t.Name(), provider, []*registry.ModelInfo{{ID: model}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(t.Name()) })
						retry := 3 * time.Hour
						m.MarkResult(ctx, Result{AuthID: t.Name(), RetryAfter: &retry, Error: &Error{HTTPStatus: 503, Message: "independent outage"}})
						if _, err := m.ApplyProviderQuotaReport(ctx, t.Name(), true, time.Now().Add(time.Hour)); err != nil {
							t.Fatal(err)
						}
						records := m.cooldownStateRecordsSnapshot()
						if len(records) != 1 || records[0].IndependentState == nil {
							t.Fatalf("missing independent state: %+v", records)
						}
						for i := range records {
							r := &records[i]
							if legacyOuter {
								r.NextRetryAfter = r.Quota.NextRecoverAt
							}
							r.NextRetryAfter = r.NextRetryAfter.Add(-elapsed)
							r.Quota.NextRecoverAt = r.Quota.NextRecoverAt.Add(-elapsed)
							r.UpdatedAt = r.UpdatedAt.Add(-elapsed)
							r.IndependentState.NextRetryAfter = r.IndependentState.NextRetryAfter.Add(-elapsed)
							r.IndependentState.UpdatedAt = r.IndependentState.UpdatedAt.Add(-elapsed)
						}
						file := NewFileCooldownStateStore(t.TempDir())
						if err := file.Save(ctx, records); err != nil {
							t.Fatal(err)
						}
						m = newManager(!delayedRegistration)
						m.SetCooldownStateStore(file)
						if err := m.RestoreCooldownStates(ctx); err != nil {
							t.Fatal(err)
						}
						if delayedRegistration {
							pending, err := file.Load(ctx)
							if err != nil || len(pending) != 1 {
								t.Fatalf("live pending record was removed: records=%+v error=%v", pending, err)
							}
							if _, err := m.Register(ctx, &Auth{ID: t.Name(), Provider: provider}); err != nil {
								t.Fatal(err)
							}
							if err := m.RestoreCooldownStates(ctx); err != nil {
								t.Fatal(err)
							}
						}
						for _, recover := range []bool{false, true} {
							if recover {
								if _, err := m.ApplyProviderQuotaRecovery(ctx, t.Name(), nil); err != nil {
									t.Fatal(err)
								}
							}
							if _, _, err := m.pickNext(ctx, provider, model, cliproxyexecutor.Options{}, nil); err == nil {
								t.Fatalf("lost independent outage after restore, recovery=%t", recover)
							}
						}
						got, _ := m.GetByID(t.Name())
						want := records[0].IndependentState.NextRetryAfter
						if !got.NextRetryAfter.Equal(want) || got.Quota.Exceeded {
							t.Fatalf("independent deadline = %v, want %v; quota=%+v", got.NextRetryAfter, want, got.Quota)
						}
					})
				}
			}
		}
	}
}
