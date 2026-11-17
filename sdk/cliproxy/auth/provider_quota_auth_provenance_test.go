package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestQuotaRecoveryPreservesCoexistingAuthCause(t *testing.T) {
	for _, capacity := range []bool{false, true} {
		durations := []time.Duration{15 * time.Minute, 3 * time.Hour}
		if capacity {
			durations = append(durations, 0)
		}
		for _, independentRetry := range durations {
			for _, quotaFirst := range []bool{false, true} {
				for _, restore := range []bool{false, true} {
					for _, reset := range []bool{false, true} {
						for _, history := range []bool{false, true} {
							t.Run(fmt.Sprintf("capacity=%t/retry=%s/quota-first=%t/restore=%t/reset=%t/history=%t", capacity, independentRetry, quotaFirst, restore, reset, history), func(t *testing.T) {
								ctx := context.Background()
								const id, model = "quota-coexisting-auth", "quota-coexisting-auth-test"
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
								quotaRetry := time.Hour
								var beforeCause, afterCause time.Time
								cause := &Error{HTTPStatus: 503, Message: "independent auth outage"}
								if capacity {
									cause = &Error{Code: ErrorCodeTransientRateLimit, HTTPStatus: 429, Message: "independent auth capacity"}
								}
								quota := func() { m.MarkResult(ctx, Result{AuthID: id, RetryAfter: &quotaRetry, Error: &Error{HTTPStatus: 429}}) }
								independent := func() {
									beforeCause = time.Now()
									m.MarkResult(ctx, Result{AuthID: id, RetryAfter: &independentRetry, Error: cause})
									afterCause = time.Now()
								}
								if quotaFirst {
									quota()
									independent()
								} else {
									independent()
									quota()
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
									_, err = m.ApplyProviderQuotaResetForModels(ctx, id, []string{}, nil)
								} else {
									_, err = m.ApplyProviderQuotaRecovery(ctx, id, nil)
								}
								if err != nil {
									t.Fatal(err)
								}
								got, _ := m.GetByID(id)
								if got.Quota.Exceeded || !got.Quota.NextRecoverAt.IsZero() {
									t.Error("recovered quota is still exhausted")
								}
								_, _, errPick := m.pickNext(ctx, "claude", model, cliproxyexecutor.Options{}, nil)
								if (errPick != nil) != (independentRetry > 0) {
									t.Errorf("independent retry=%s pick=%v", independentRetry, errPick)
								}
								if independentRetry > 0 {
									if got.NextRetryAfter.Before(beforeCause.Add(independentRetry)) || got.NextRetryAfter.After(afterCause.Add(independentRetry)) {
										t.Errorf("independent retry was replaced by quota timing: next=%v want=%s", got.NextRetryAfter, independentRetry)
									}
									if !cooldownErrorEqual(got.LastError, cause) || got.Status != StatusError {
										t.Errorf("independent cause lost: error=%v status=%s", got.LastError, got.Status)
									}
								}
								records, err := file.Load(ctx)
								if err != nil {
									t.Fatal(err)
								}
								if (len(records) > 0) != (independentRetry > 0) {
									t.Errorf("independent retry=%s persisted records=%d", independentRetry, len(records))
								}
								if blocked, _, _ := isAuthBlockedForModel(got, model, afterCause.Add(independentRetry+time.Second)); independentRetry > 0 && blocked {
									t.Error("recovered quota inflated the independent deadline")
								}
							})
						}
					}
				}
			}
		}
	}
}

func TestQuotaDelayedRestoreRespectsCredentialLifetime(t *testing.T) {
	for _, sequence := range []string{"unchanged", "remove_reregister", "register_after_read", "register_remove_register_after_read"} {
		t.Run(sequence, func(t *testing.T) {
			ctx := context.Background()
			const a, b, model = "lifetime-a", "lifetime-b", "lifetime-force"
			donor := NewManager(nil, nil, nil)
			for _, id := range []string{a, b} {
				if _, err := donor.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := donor.ApplyProviderQuotaReport(ctx, a, true, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			retry := 3 * time.Hour
			donor.MarkResult(ctx, Result{AuthID: b, Model: model, RetryAfter: &retry, Error: &Error{Code: ErrorCodeForceCooldown, HTTPStatus: 503}})
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
			if _, err := m.ApplyProviderQuotaReset(ctx, a, []string{"a-base"}, nil); err != nil {
				t.Fatal(err)
			}
			registerB := func() {
				if _, err := m.Register(ctx, &Auth{ID: b, Provider: "claude"}); err != nil {
					t.Fatal(err)
				}
			}
			if sequence == "unchanged" || sequence == "remove_reregister" {
				registerB()
			}
			gate := &gatedQuotaFileLoad{FileCooldownStateStore: file, loaded: make(chan struct{}), release: make(chan struct{})}
			m.SetCooldownStateStore(gate)
			done := make(chan error, 1)
			go func() { done <- m.RestoreCooldownStates(ctx) }()
			<-gate.loaded
			if sequence == "register_after_read" || sequence == "register_remove_register_after_read" {
				registerB()
			}
			removed := sequence == "remove_reregister" || sequence == "register_remove_register_after_read"
			if removed {
				m.Remove(ctx, b)
				registerB()
			}
			close(gate.release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(b, "claude", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(b) })
			_, _, errPick := m.pickNext(ctx, "claude", model, cliproxyexecutor.Options{}, nil)
			if (errPick != nil) == removed {
				t.Errorf("old load crossed credential lifetime: removed=%t pick=%v", removed, errPick)
			}
			records, err := file.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, record := range records {
				if record.AuthID == b {
					found = true
				}
			}
			if found == removed {
				t.Errorf("persisted lifetime mismatch: removed=%t B-records=%t", removed, found)
			}
		})
	}
}

func TestQuotaRecoveryAdoptsLegacyStandaloneAuthCause(t *testing.T) {
	ctx := context.Background()
	const id = "legacy-standalone-cause"
	deadline := time.Now().Add(3 * time.Hour)
	file := NewFileCooldownStateStore(t.TempDir())
	if err := file.Save(ctx, []CooldownStateRecord{{AuthID: id, Provider: "claude", NextRetryAfter: deadline,
		LastError: &Error{HTTPStatus: 503, Message: "legacy outage"}, UpdatedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil, nil, nil)
	if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	m.SetCooldownStateStore(file)
	if err := m.RestoreCooldownStates(ctx); err != nil {
		t.Fatal(err)
	}
	retry := time.Hour
	m.MarkResult(ctx, Result{AuthID: id, RetryAfter: &retry, Error: &Error{HTTPStatus: 429}})
	if _, err := m.ApplyProviderQuotaRecovery(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetByID(id)
	if blocked, _, next := isAuthBlockedForModel(got, "model", time.Now()); !blocked || !next.Equal(deadline) {
		t.Fatalf("quota overwrote an attributable legacy auth cause: blocked=%t next=%v want=%v", blocked, next, deadline)
	}
}

func TestQuotaDelayedRestorePreservesNewZeroCapacity(t *testing.T) {
	ctx := context.Background()
	const id = "delayed-zero-auth-cause"
	donor := NewManager(nil, nil, nil)
	if _, err := donor.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	retry := time.Hour
	donor.MarkResult(ctx, Result{AuthID: id, RetryAfter: &retry, Error: &Error{HTTPStatus: 429}})
	retry = 3 * time.Hour
	donor.MarkResult(ctx, Result{AuthID: id, RetryAfter: &retry, Error: &Error{Code: ErrorCodeTransientRateLimit, HTTPStatus: 429}})
	file := NewFileCooldownStateStore(t.TempDir())
	if err := file.Save(ctx, donor.cooldownStateRecordsSnapshot()); err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil, nil, nil)
	if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	gate := &gatedQuotaFileLoad{FileCooldownStateStore: file, loaded: make(chan struct{}), release: make(chan struct{})}
	m.SetCooldownStateStore(gate)
	done := make(chan error, 1)
	go func() { done <- m.RestoreCooldownStates(ctx) }()
	<-gate.loaded
	retry = 0
	m.MarkResult(ctx, Result{AuthID: id, RetryAfter: &retry, Error: &Error{Code: ErrorCodeTransientRateLimit, HTTPStatus: 429}})
	_, errRecover := m.ApplyProviderQuotaRecovery(ctx, id, nil)
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if errRecover != nil {
		t.Fatal(errRecover)
	}
	got, _ := m.GetByID(id)
	if blocked, _, _ := isAuthBlockedForModel(got, "model", time.Now()); blocked {
		t.Fatal("delayed disk snapshot undid the newer explicit zero-capacity result")
	}
}

func TestQuotaDelayedRestorePreservesSuccessfulAuthRecovery(t *testing.T) {
	ctx := context.Background()
	const id = "delayed-success-auth-cause"
	donor := NewManager(nil, nil, nil)
	if _, err := donor.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	retry := 3 * time.Hour
	donor.MarkResult(ctx, Result{AuthID: id, RetryAfter: &retry, Error: &Error{HTTPStatus: 503}})
	file := NewFileCooldownStateStore(t.TempDir())
	if err := file.Save(ctx, donor.cooldownStateRecordsSnapshot()); err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil, nil, nil)
	if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	gate := &gatedQuotaFileLoad{FileCooldownStateStore: file, loaded: make(chan struct{}), release: make(chan struct{})}
	m.SetCooldownStateStore(gate)
	done := make(chan error, 1)
	go func() { done <- m.RestoreCooldownStates(ctx) }()
	<-gate.loaded
	m.MarkResult(ctx, Result{AuthID: id, Model: "model", Success: true, ExecutionSeq: m.nextExecutionSeq()})
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetByID(id)
	if blocked, _, _ := isAuthBlockedForModel(got, "model", time.Now()); blocked {
		t.Fatal("delayed disk snapshot undid a newer successful auth recovery")
	}
}
