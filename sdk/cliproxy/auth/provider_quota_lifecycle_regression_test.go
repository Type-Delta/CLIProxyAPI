package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestScopedQuotaRestoreJoinsIndependentLayers(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, recordScoped := range []bool{false, true} {
			t.Run(fmt.Sprintf("shared=%t/record-scoped=%t", shared, recordScoped), func(t *testing.T) {
				ctx := context.Background()
				now := time.Now().UTC()
				deadline := now.Add(3 * time.Hour)
				newManager := func(scoped bool) *Manager {
					m := NewManager(nil, nil, nil)
					state := &ModelState{Status: StatusError, Unavailable: true, NextRetryAfter: deadline,
						LastError: &Error{HTTPStatus: 503, Message: "independent outage"}, UpdatedAt: now.Add(time.Minute)}
					if scoped {
						state = &ModelState{Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(30 * time.Minute),
							LastError: &Error{HTTPStatus: 502, Message: "older independent outage"}, UpdatedAt: now.Add(-time.Hour)}
					}
					if _, err := m.Register(ctx, &Auth{ID: "scoped-restore", Provider: "claude", ModelStates: map[string]*ModelState{"fable": state}}); err != nil {
						t.Fatal(err)
					}
					if scoped {
						if _, err := m.ApplyProviderQuotaReportForModels(ctx, "scoped-restore", true, now.Add(time.Hour), []string{"fable"}); err != nil {
							t.Fatal(err)
						}
						if shared {
							if _, err := m.ApplyProviderQuotaReport(ctx, "scoped-restore", true, now.Add(2*time.Hour)); err != nil {
								t.Fatal(err)
							}
						}
					}
					return m
				}
				donor := newManager(recordScoped)
				store := NewFileCooldownStateStore(t.TempDir())
				if err := store.Save(ctx, donor.cooldownStateRecordsSnapshot()); err != nil {
					t.Fatal(err)
				}
				receiver := newManager(!recordScoped)
				receiver.SetCooldownStateStore(store)
				if err := receiver.RestoreCooldownStates(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := receiver.ApplyProviderQuotaRecovery(ctx, "scoped-restore", []string{"fable"}); err != nil {
					t.Fatal(err)
				}
				got, _ := receiver.GetByID("scoped-restore")
				state := got.ModelStates["fable"]
				if state.LastError == nil || state.LastError.HTTPStatus != 503 || !state.NextRetryAfter.Equal(deadline) {
					t.Fatalf("restore/recovery lost independent 503: %+v", state)
				}
				if blocked, _, next := isAuthBlockedForModel(got, "fable", now.Add(2*time.Hour)); !blocked || !next.Equal(deadline) {
					t.Fatalf("independent routing deadline: blocked=%t next=%v, want %v", blocked, next, deadline)
				}
			})
		}
	}
}

func TestScopedQuotaDisableCoolingPreservesExactForce(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("shared=%t/restart=%t", shared, restart), func(t *testing.T) {
				ctx := context.Background()
				m := NewManager(nil, nil, nil)
				const id = "scoped-force"
				if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
					t.Fatal(err)
				}
				now := time.Now()
				if _, err := m.ApplyProviderQuotaReportForModels(ctx, id, true, now.Add(7*time.Hour), []string{"fable"}); err != nil {
					t.Fatal(err)
				}
				if shared {
					if _, err := m.ApplyProviderQuotaReport(ctx, id, true, now.Add(2*time.Hour)); err != nil {
						t.Fatal(err)
					}
				}
				retry := 3 * time.Hour
				m.MarkResult(ctx, Result{AuthID: id, Model: "fable", RetryAfter: &retry, Error: &Error{Code: ErrorCodeForceCooldown, HTTPStatus: 503}})
				forced, _ := m.GetByID(id)
				independent := forced.ModelStates["fable"]
				if shared {
					independent = independent.credentialQuotaShadow
				}
				deadline := independent.scopedQuotaShadow.NextRetryAfter
				retry = 5 * time.Hour
				m.MarkResult(ctx, Result{AuthID: id, Model: "fable", RetryAfter: &retry, Error: &Error{HTTPStatus: 503}})
				m.MarkResult(ctx, Result{AuthID: id, Model: "opus", RetryAfter: &retry, Error: &Error{HTTPStatus: 503}})
				if restart {
					store := NewFileCooldownStateStore(t.TempDir())
					if err := store.Save(ctx, m.cooldownStateRecordsSnapshot()); err != nil {
						t.Fatal(err)
					}
					m = NewManager(nil, nil, nil)
					m.SetConfig(&internalconfig.Config{DisableCooling: true})
					if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
						t.Fatal(err)
					}
					m.SetCooldownStateStore(store)
					if err := m.RestoreCooldownStates(ctx); err != nil {
						t.Fatal(err)
					}
				} else {
					m.SetConfig(&internalconfig.Config{DisableCooling: true})
				}
				got, _ := m.GetByID(id)
				if blocked, _, next := isAuthBlockedForModel(got, "fable", now); !blocked || !next.Equal(deadline) {
					t.Fatalf("disable-cooling lost exact force: blocked=%t next=%v want=%v state=%+v", blocked, next, deadline, got.ModelStates["fable"])
				}
				if blocked, _, _ := isAuthBlockedForModel(got, "fable", deadline.Add(time.Second)); blocked {
					t.Fatal("ordinary extension survived force expiry")
				}
				if blocked, _, _ := isAuthBlockedForModel(got, "opus", now); blocked {
					t.Fatal("ordinary sibling cooling survived disable-cooling")
				}
			})
		}
	}
}

func TestScopedQuotaLateSuccessAfterSharedExpiry(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	const id = "scoped-late-success"
	if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude", ModelStates: map[string]*ModelState{"fable": {Status: StatusActive}}}); err != nil {
		t.Fatal(err)
	}
	seq := m.nextExecutionSeq()
	now := time.Now()
	deadline := now.Add(3 * time.Hour)
	if _, err := m.ApplyProviderQuotaReport(ctx, id, true, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApplyProviderQuotaReportForModels(ctx, id, true, deadline, []string{"fable"}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a := m.auths[id]
	a.Quota.NextRecoverAt = now.Add(-time.Hour)
	a.NextRetryAfter = now.Add(-time.Hour)
	a.ModelStates["fable"].NextRetryAfter = now.Add(-time.Hour)
	a.ModelStates["fable"].Quota.NextRecoverAt = now.Add(-time.Hour)
	m.mu.Unlock()
	m.MarkResult(ctx, Result{AuthID: id, Model: "fable", Success: true, CompletedResponse: true, ExecutionSeq: seq})
	got, _ := m.GetByID(id)
	if blocked, _, next := isAuthBlockedForModel(got, "fable", now); !blocked || !next.Equal(deadline) {
		t.Fatalf("late success cleared inner quota: blocked=%t next=%v state=%+v", blocked, next, got.ModelStates["fable"])
	}
	if got.InferenceVerified {
		t.Fatal("late success incorrectly verified inference through active scoped quota")
	}
}

func TestScopedQuotaRecoveryPreservesTransientCapacity(t *testing.T) {
	for _, reset := range []bool{false, true} {
		for _, shared := range []bool{false, true} {
			t.Run(fmt.Sprintf("reset=%t/shared=%t", reset, shared), func(t *testing.T) {
				ctx := context.Background()
				m := NewManager(nil, nil, nil)
				const id = "scoped-transient"
				if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
					t.Fatal(err)
				}
				retry := 3 * time.Hour
				m.MarkResult(ctx, Result{AuthID: id, Model: "fable", RetryAfter: &retry, Error: &Error{Code: ErrorCodeTransientRateLimit, HTTPStatus: 429}})
				before, _ := m.GetByID(id)
				deadline := before.ModelStates["fable"].NextRetryAfter
				if _, err := m.ApplyProviderQuotaReportForModels(ctx, id, true, time.Now().Add(time.Hour), []string{"fable"}); err != nil {
					t.Fatal(err)
				}
				if shared {
					if _, err := m.ApplyProviderQuotaReport(ctx, id, true, time.Now().Add(2*time.Hour)); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				if reset {
					_, err = m.ApplyProviderQuotaReset(ctx, id, []string{"fable"}, nil)
				} else {
					_, err = m.ApplyProviderQuotaRecovery(ctx, id, []string{"fable"})
				}
				if err != nil {
					t.Fatal(err)
				}
				got, _ := m.GetByID(id)
				state := got.ModelStates["fable"]
				if state.LastError == nil || state.LastError.Code != ErrorCodeTransientRateLimit || state.Quota.Exceeded || !state.NextRetryAfter.Equal(deadline) {
					t.Fatalf("quota recovery erased transient capacity: %+v", state)
				}
				if blocked, _, next := isAuthBlockedForModel(got, "fable", time.Now()); !blocked || !next.Equal(deadline) {
					t.Fatalf("transient routing: blocked=%t next=%v want=%v", blocked, next, deadline)
				}
			})
		}
	}
}

func TestScopedQuotaLateSuccessRetainsIndependentWatermark(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	const id = "scoped-late-independent"
	if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	seq := m.nextExecutionSeq()
	retry := 3 * time.Hour
	m.MarkResult(ctx, Result{AuthID: id, Model: "fable", RetryAfter: &retry, Error: &Error{HTTPStatus: 503}})
	before, _ := m.GetByID(id)
	deadline := before.ModelStates["fable"].NextRetryAfter
	if _, err := m.ApplyProviderQuotaReportForModels(ctx, id, true, time.Now().Add(time.Hour), []string{"fable"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApplyProviderQuotaReport(ctx, id, true, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a := m.auths[id]
	expired := time.Now().Add(-time.Hour)
	a.Quota.NextRecoverAt, a.NextRetryAfter = expired, expired
	state := a.ModelStates["fable"]
	state.Quota.NextRecoverAt, state.NextRetryAfter = expired, expired
	state.credentialQuotaShadow.Quota.NextRecoverAt, state.credentialQuotaShadow.NextRetryAfter = expired, expired
	m.mu.Unlock()
	m.MarkResult(ctx, Result{AuthID: id, Model: "fable", Success: true, CompletedResponse: true, ExecutionSeq: seq})
	got, _ := m.GetByID(id)
	if blocked, _, next := isAuthBlockedForModel(got, "fable", time.Now()); !blocked || !next.Equal(deadline) {
		t.Fatalf("late success cleared independent watermark: blocked=%t next=%v want=%v", blocked, next, deadline)
	}
	if got.InferenceVerified {
		t.Fatal("late success incorrectly verified inference through independent cooldown")
	}
}

func TestScopedQuotaDisableCoolingRetainsSharedAndIndependentForce(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			ctx := context.Background()
			m := NewManager(nil, nil, nil)
			const id = "scoped-shared-force"
			if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude", ModelStates: map[string]*ModelState{
				"fable": {Status: StatusActive}, "opus": {Status: StatusActive},
			}}); err != nil {
				t.Fatal(err)
			}
			retry := 2 * time.Hour
			m.MarkResult(ctx, Result{AuthID: id, Model: "opus", CredentialScope: true, RetryAfter: &retry, Error: &Error{Code: ErrorCodeForceCooldown, HTTPStatus: 429}})
			before, _ := m.GetByID(id)
			sharedDeadline := before.Quota.NextRecoverAt
			if _, err := m.ApplyProviderQuotaReportForModels(ctx, id, true, time.Now().Add(7*time.Hour), []string{"fable"}); err != nil {
				t.Fatal(err)
			}
			retry = 3 * time.Hour
			m.MarkResult(ctx, Result{AuthID: id, Model: "fable", RetryAfter: &retry, Error: &Error{Code: ErrorCodeForceCooldown, HTTPStatus: 503}})
			before, _ = m.GetByID(id)
			independentDeadline := before.ModelStates["fable"].credentialQuotaShadow.scopedQuotaShadow.NextRetryAfter
			retry = 5 * time.Hour
			for _, model := range []string{"fable", "opus"} {
				m.MarkResult(ctx, Result{AuthID: id, Model: model, RetryAfter: &retry, Error: &Error{HTTPStatus: 503}})
			}
			if restart {
				store := NewFileCooldownStateStore(t.TempDir())
				if err := store.Save(ctx, m.cooldownStateRecordsSnapshot()); err != nil {
					t.Fatal(err)
				}
				m = NewManager(nil, nil, nil)
				m.SetConfig(&internalconfig.Config{DisableCooling: true})
				if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
					t.Fatal(err)
				}
				m.SetCooldownStateStore(store)
				if err := m.RestoreCooldownStates(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				m.SetConfig(&internalconfig.Config{DisableCooling: true})
			}
			got, _ := m.GetByID(id)
			for model, deadline := range map[string]time.Time{"fable": independentDeadline, "opus": sharedDeadline} {
				if blocked, _, next := isAuthBlockedForModel(got, model, time.Now()); !blocked || !next.Equal(deadline) {
					t.Fatalf("%s force bounds: blocked=%t next=%v want=%v", model, blocked, next, deadline)
				}
				if blocked, _, _ := isAuthBlockedForModel(got, model, deadline.Add(time.Second)); blocked {
					t.Fatalf("%s ordinary cooldown survived force expiry", model)
				}
			}
		})
	}
}
