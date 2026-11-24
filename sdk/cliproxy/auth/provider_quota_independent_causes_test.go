package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestQuotaRecoveryClearsQuotaBesideShorterModelForce(t *testing.T) {
	for _, forceFirst := range []bool{true, false} {
		for _, restore := range []bool{false, true} {
			for _, reset := range []bool{false, true} {
				t.Run(fmt.Sprintf("force-first=%t/restore=%t/reset=%t", forceFirst, restore, reset), func(t *testing.T) {
					ctx := context.Background()
					const id, model = "quota-model-force", "quota-model-force-test"
					newManager := func() *Manager {
						m := NewManager(nil, nil, nil)
						m.RegisterExecutor(&refreshMockExecutor{id: "claude"})
						if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
							t.Fatal(err)
						}
						return m
					}
					m := newManager()
					reg := registry.GetGlobalRegistry()
					reg.RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { reg.UnregisterClient(id) })
					forceRetry, quotaRetry := time.Hour, 3*time.Hour
					force := func() {
						m.MarkResult(ctx, Result{AuthID: id, Model: model, RetryAfter: &forceRetry, Error: &Error{Code: ErrorCodeForceCooldown, HTTPStatus: 503}})
					}
					quota := func() {
						m.MarkResult(ctx, Result{AuthID: id, Model: model, RetryAfter: &quotaRetry, Error: &Error{HTTPStatus: 429}})
					}
					if forceFirst {
						force()
						quota()
					} else {
						quota()
						force()
					}
					before, _ := m.GetByID(id)
					state := before.ModelStates[model]
					deadline := effectiveModelForceUntil(state)
					if state.scopedQuotaShadow != nil {
						deadline = effectiveModelForceUntil(state.scopedQuotaShadow)
					}
					file := NewFileCooldownStateStore(t.TempDir())
					if err := file.Save(ctx, m.cooldownStateRecordsSnapshot()); err != nil {
						t.Fatal(err)
					}
					if restore {
						m = newManager()
					}
					m.SetCooldownStateStore(file)
					if restore {
						if err := m.RestoreCooldownStates(ctx); err != nil {
							t.Fatal(err)
						}
					}
					var err error
					if reset {
						_, err = m.ApplyProviderQuotaReset(ctx, id, []string{model}, nil)
					} else {
						_, err = m.ApplyProviderQuotaRecovery(ctx, id, []string{model})
					}
					if err != nil {
						t.Fatal(err)
					}
					got, _ := m.GetByID(id)
					state = got.ModelStates[model]
					if state.Quota.Exceeded || !state.Quota.NextRecoverAt.IsZero() || !state.NextRetryAfter.Equal(deadline) {
						t.Errorf("recovery kept quota beyond independent force: state=%+v force=%v", state, deadline)
					}
					if _, _, errPick := m.pickNext(ctx, "claude", model, cliproxyexecutor.Options{}, nil); errPick == nil {
						t.Error("recovery released the still-active explicit force")
					}
					if blocked, _, next := isAuthBlockedForModel(got, model, deadline.Add(time.Second)); blocked {
						t.Errorf("recovered quota blocks after force expires: next=%v", next)
					}
					// Expire only the independent bound; a retained 3h quota must
					// still fail the real routing path instead of being erased here.
					m.mu.Lock()
					auth := m.auths[id]
					expired := time.Now().Add(-time.Second)
					auth.ModelStates[model].forceCooldownUntil = expired
					if auth.ModelStates[model].NextRetryAfter.Equal(deadline) {
						auth.ModelStates[model].NextRetryAfter = expired
					}
					if auth.NextRetryAfter.Equal(deadline) {
						auth.NextRetryAfter = expired
					}
					m.mu.Unlock()
					m.RefreshSchedulerEntry(id)
					if _, _, errPick := m.pickNext(ctx, "claude", model, cliproxyexecutor.Options{}, nil); errPick != nil {
						t.Errorf("pick after force expiry: %v", errPick)
					}
				})
			}
		}
	}
}

func TestQuotaNoopRecoveryPreservesIndependentAuthCooldown(t *testing.T) {
	for _, capacity := range []bool{false, true} {
		for _, history := range []bool{false, true} {
			for _, restore := range []bool{false, true} {
				for _, reset := range []bool{false, true} {
					t.Run(fmt.Sprintf("capacity=%t/history=%t/restore=%t/reset=%t", capacity, history, restore, reset), func(t *testing.T) {
						ctx := context.Background()
						const id, model = "quota-noop-auth", "quota-noop-auth-test"
						newManager := func() *Manager {
							m := NewManager(nil, nil, nil)
							m.RegisterExecutor(&refreshMockExecutor{id: "claude"})
							auth := &Auth{ID: id, Provider: "claude"}
							if history {
								auth.ModelStates = map[string]*ModelState{model: {Status: StatusActive}}
							}
							if _, err := m.Register(ctx, auth); err != nil {
								t.Fatal(err)
							}
							return m
						}
						m := newManager()
						reg := registry.GetGlobalRegistry()
						reg.RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
						t.Cleanup(func() { reg.UnregisterClient(id) })
						retry := 3 * time.Hour
						resultErr := &Error{HTTPStatus: 503, Message: "independent auth outage"}
						if capacity {
							resultErr = &Error{Code: ErrorCodeTransientRateLimit, HTTPStatus: 429, Message: "independent auth capacity"}
						}
						m.MarkResult(ctx, Result{AuthID: id, RetryAfter: &retry, Error: resultErr})
						file := NewFileCooldownStateStore(t.TempDir())
						if err := file.Save(ctx, m.cooldownStateRecordsSnapshot()); err != nil {
							t.Fatal(err)
						}
						if restore {
							m = newManager()
						}
						m.SetCooldownStateStore(file)
						if restore {
							if err := m.RestoreCooldownStates(ctx); err != nil {
								t.Fatal(err)
							}
						}
						before, _ := m.GetByID(id)
						_, _, beforePick := m.pickNext(ctx, "claude", model, cliproxyexecutor.Options{}, nil)
						var err error
						if reset {
							_, err = m.ApplyProviderQuotaResetForModels(ctx, id, []string{}, nil)
						} else {
							_, err = m.ApplyProviderQuotaRecovery(ctx, id, nil)
						}
						if err != nil {
							t.Fatal(err)
						}
						got, _ := m.GetByID(id)
						if !got.NextRetryAfter.Equal(before.NextRetryAfter) || got.Unavailable != before.Unavailable || !cooldownErrorEqual(got.LastError, before.LastError) || got.Status != before.Status {
							t.Errorf("no-quota recovery changed independent auth cooldown: before=%+v after=%+v", before, got)
						}
						_, _, afterPick := m.pickNext(ctx, "claude", model, cliproxyexecutor.Options{}, nil)
						if (beforePick != nil) != (afterPick != nil) {
							t.Errorf("no-quota recovery changed routing: before=%v after=%v", beforePick, afterPick)
						}
						records, err := file.Load(ctx)
						if err != nil {
							t.Fatal(err)
						}
						if len(records) == 0 {
							t.Error("no-quota recovery deleted the independent persisted auth cooldown")
						}
					})
				}
			}
		}
	}
}

func TestQuotaRecoveryRetainsRegisteredUnreadCredentialPools(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprintf("remove=%t", remove), func(t *testing.T) {
			ctx := context.Background()
			const a, b = "quota-handoff-a", "quota-handoff-b"
			const base, remaining = "a-base", "a-remaining"
			const force, capacity, scoped = "b-force", "b-capacity", "b-scoped"
			donor := NewManager(nil, nil, nil)
			for _, id := range []string{a, b} {
				if _, err := donor.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now()
			if _, err := donor.ApplyProviderQuotaReport(ctx, a, true, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err := donor.ApplyProviderQuotaReportForModels(ctx, a, true, now.Add(2*time.Hour), []string{remaining}); err != nil {
				t.Fatal(err)
			}
			retry := 3 * time.Hour
			donor.MarkResult(ctx, Result{AuthID: b, Model: force, RetryAfter: &retry, Error: &Error{Code: ErrorCodeForceCooldown, HTTPStatus: 503}})
			donor.MarkResult(ctx, Result{AuthID: b, Model: capacity, RetryAfter: &retry, Error: &Error{Code: ErrorCodeTransientRateLimit, HTTPStatus: 429}})
			if _, err := donor.ApplyProviderQuotaReportForModels(ctx, b, true, now.Add(3*time.Hour), []string{scoped}); err != nil {
				t.Fatal(err)
			}
			file := NewFileCooldownStateStore(t.TempDir())
			if err := file.Save(ctx, donor.cooldownStateRecordsSnapshot()); err != nil {
				t.Fatal(err)
			}
			m := NewManager(nil, nil, nil)
			m.RegisterExecutor(&refreshMockExecutor{id: "claude"})
			if _, err := m.Register(ctx, &Auth{ID: a, Provider: "claude"}); err != nil {
				t.Fatal(err)
			}
			m.SetCooldownStateStore(file)
			if _, err := m.ApplyProviderQuotaReset(ctx, a, []string{base}, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Register(ctx, &Auth{ID: b, Provider: "claude"}); err != nil {
				t.Fatal(err)
			}
			if remove {
				m.Remove(ctx, b)
				if _, err := m.Register(ctx, &Auth{ID: b, Provider: "claude"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.ApplyProviderQuotaRecovery(ctx, a, []string{remaining}); err != nil {
				t.Fatal(err)
			}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(b, "claude", []*registry.ModelInfo{{ID: force}, {ID: capacity}, {ID: scoped}})
			t.Cleanup(func() { reg.UnregisterClient(b) })
			if err := m.RestoreCooldownStates(ctx); err != nil {
				t.Fatal(err)
			}
			for _, model := range []string{force, capacity, scoped} {
				_, _, errPick := m.pickNext(ctx, "claude", model, cliproxyexecutor.Options{}, nil)
				if (errPick == nil) != remove {
					t.Errorf("registered-unrestored %s: removed=%t pick=%v", model, remove, errPick)
				}
			}
		})
	}
}

func TestQuotaRecoveryStillClearsAuthQuotaAfterZeroCapacity(t *testing.T) {
	for _, history := range []bool{false, true} {
		for _, reset := range []bool{false, true} {
			t.Run(fmt.Sprintf("history=%t/reset=%t", history, reset), func(t *testing.T) {
				ctx := context.Background()
				const id, model = "quota-auth-zero-capacity", "quota-auth-zero-capacity-test"
				m := NewManager(nil, nil, nil)
				m.RegisterExecutor(&refreshMockExecutor{id: "claude"})
				auth := &Auth{ID: id, Provider: "claude"}
				if history {
					auth.ModelStates = map[string]*ModelState{model: {Status: StatusActive}}
				}
				if _, err := m.Register(ctx, auth); err != nil {
					t.Fatal(err)
				}
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { reg.UnregisterClient(id) })
				retry := 3 * time.Hour
				m.MarkResult(ctx, Result{AuthID: id, RetryAfter: &retry, Error: &Error{HTTPStatus: 429}})
				retry = 0
				m.MarkResult(ctx, Result{AuthID: id, RetryAfter: &retry, Error: &Error{Code: ErrorCodeTransientRateLimit, HTTPStatus: 429}})
				var err error
				if reset {
					_, err = m.ApplyProviderQuotaResetForModels(ctx, id, []string{}, nil)
				} else {
					_, err = m.ApplyProviderQuotaRecovery(ctx, id, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				got, _ := m.GetByID(id)
				if got.Quota.Exceeded || !got.Quota.NextRecoverAt.IsZero() {
					t.Error("independent-cause preservation masked genuine quota cleanup")
				}
				if _, _, errPick := m.pickNext(ctx, "claude", model, cliproxyexecutor.Options{}, nil); errPick != nil {
					t.Fatalf("zero-capacity retry prevented immediate routing after quota recovery: %v", errPick)
				}
			})
		}
	}
}

func TestQuotaRecoveryRetainsModelForceSuccessWatermark(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	const id, model = "quota-force-watermark", "quota-force-watermark-test"
	if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	oldSeq := m.nextExecutionSeq()
	retry := time.Hour
	m.MarkResult(ctx, Result{AuthID: id, Model: model, RetryAfter: &retry, Error: &Error{Code: ErrorCodeForceCooldown, HTTPStatus: 503}})
	before, _ := m.GetByID(id)
	deadline := effectiveModelForceUntil(before.ModelStates[model])
	m.nextExecutionSeq()
	retry = 3 * time.Hour
	m.MarkResult(ctx, Result{AuthID: id, Model: model, RetryAfter: &retry, Error: &Error{HTTPStatus: 429}})
	if _, err := m.ApplyProviderQuotaRecovery(ctx, id, []string{model}); err != nil {
		t.Fatal(err)
	}
	m.MarkResult(ctx, Result{AuthID: id, Model: model, ExecutionSeq: oldSeq, Success: true})
	got, _ := m.GetByID(id)
	if blocked, _, next := isAuthBlockedForModel(got, model, time.Now()); !blocked || !next.Equal(deadline) {
		t.Fatalf("pre-force success cleared remaining force after quota recovery: blocked=%t next=%v want=%v", blocked, next, deadline)
	}
}
