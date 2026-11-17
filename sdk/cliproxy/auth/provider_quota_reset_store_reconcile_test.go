package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestQuotaRecoveryReconcilesPersistedUnselectedLayers(t *testing.T) {
	for _, kind := range []string{"force", "capacity", "scoped"} {
		for _, order := range []string{"restore_then_reset", "delayed_restore_reset", "reset_before_restore", "delayed_restore_healthy", "partial_then_shared_healthy", "fable_then_shared_healthy", "multiple_partial_resets"} {
			t.Run(kind+"/"+order, func(t *testing.T) {
				ctx := context.Background()
				const id, otherID = "quota-store-scopes", "quota-store-other"
				const base, fable = "claude-opus-5", "claude-fable-5-2"
				newManager := func() *Manager {
					m := NewManager(nil, nil, nil)
					m.RegisterExecutor(&refreshMockExecutor{id: "claude"})
					for _, authID := range []string{id, otherID} {
						if _, err := m.Register(ctx, &Auth{ID: authID, Provider: "claude", ModelStates: map[string]*ModelState{base: {Status: StatusActive}, fable: {Status: StatusActive}}}); err != nil {
							t.Fatal(err)
						}
					}
					return m
				}
				donor := newManager()
				retry := 3 * time.Hour
				if kind == "scoped" {
					if _, err := donor.ApplyProviderQuotaReportForModels(ctx, id, true, time.Now().Add(retry), []string{fable}); err != nil {
						t.Fatal(err)
					}
				} else {
					code, status := ErrorCodeForceCooldown, 503
					if kind == "capacity" {
						code, status = ErrorCodeTransientRateLimit, 429
					}
					donor.MarkResult(ctx, Result{AuthID: id, Model: fable, RetryAfter: &retry, Error: &Error{Code: code, HTTPStatus: status}})
				}
				independent, _ := donor.GetByID(id)
				fableDeadline := independent.ModelStates[fable].NextRetryAfter
				if _, err := donor.ApplyProviderQuotaReport(ctx, id, true, time.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				donor.MarkResult(ctx, Result{AuthID: otherID, Model: base, RetryAfter: &retry, Error: &Error{HTTPStatus: 503}})
				file := NewFileCooldownStateStore(t.TempDir())
				if err := file.Save(ctx, donor.cooldownStateRecordsSnapshot()); err != nil {
					t.Fatal(err)
				}
				m := newManager()
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(id, "claude", []*registry.ModelInfo{{ID: base}, {ID: fable}})
				t.Cleanup(func() { reg.UnregisterClient(id) })
				var done chan error
				var release chan struct{}
				if order == "restore_then_reset" || order == "reset_before_restore" {
					m.SetCooldownStateStore(file)
					if order == "restore_then_reset" {
						if err := m.RestoreCooldownStates(ctx); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					gate := &gatedQuotaFileLoad{FileCooldownStateStore: file, loaded: make(chan struct{}), release: make(chan struct{})}
					m.SetCooldownStateStore(gate)
					done, release = make(chan error, 1), gate.release
					go func() { done <- m.RestoreCooldownStates(ctx) }()
					<-gate.loaded
				}
				var err error
				if order == "delayed_restore_healthy" {
					_, err = m.ApplyProviderQuotaRecovery(ctx, id, []string{base})
				} else if order == "partial_then_shared_healthy" || order == "fable_then_shared_healthy" {
					models := []string{base}
					if order == "fable_then_shared_healthy" {
						models = []string{fable}
					}
					_, err = m.ApplyProviderQuotaResetForModels(ctx, id, models, nil)
					if err == nil {
						observation := m.CaptureQuotaObservation(ctx, id)
						epoch := m.QuotaObservationEpoch(id)
						_, err = m.ApplyProviderQuotaRecovery(observation, id, []string{})
						if m.QuotaObservationEpoch(id) != epoch {
							t.Error("healthy shared recovery invalidated later stages of the same quota response")
						}
						if err == nil {
							_, err = m.ApplyProviderQuotaReportForModels(observation, id, true, time.Now().Add(time.Hour), []string{"same-response-family"})
							current, _ := m.GetByID(id)
							if state := current.ModelStates["same-response-family"]; state == nil || !state.Quota.Exceeded {
								t.Error("later family stage of the same usage response was rejected as stale")
							}
						}
					}
				} else {
					_, err = m.ApplyProviderQuotaReset(ctx, id, []string{base}, nil)
					if err == nil && order == "multiple_partial_resets" {
						_, err = m.ApplyProviderQuotaResetForModels(ctx, id, []string{fable}, nil)
					}
				}
				// New live failures must not be cleared when the older load returns.
				retry = 4 * time.Hour
				m.MarkResult(ctx, Result{AuthID: id, Model: "later-live", RetryAfter: &retry, Error: &Error{HTTPStatus: 503}})
				if release != nil {
					close(release)
					if errRestore := <-done; errRestore != nil {
						t.Fatal(errRestore)
					}
				} else if order == "reset_before_restore" {
					if errRestore := m.RestoreCooldownStates(ctx); errRestore != nil {
						t.Fatal(errRestore)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				got, _ := m.GetByID(id)
				if _, _, errPick := m.pickNext(ctx, "claude", base, cliproxyexecutor.Options{}, nil); errPick != nil {
					t.Errorf("recovered base allowance is not routable: %v", errPick)
				}
				fableRecovered := kind == "scoped" && (order == "fable_then_shared_healthy" || order == "multiple_partial_resets")
				if blocked, _, next := isAuthBlockedForModel(got, fable, time.Now()); blocked == fableRecovered || blocked && !next.Equal(fableDeadline) {
					t.Errorf("Fable %s recovery=%t: blocked=%t next=%v original=%v", kind, fableRecovered, blocked, next, fableDeadline)
				}
				if blocked, _, _ := isAuthBlockedForModel(got, "later-live", time.Now()); !blocked {
					t.Error("delayed restore lost a newer live failure")
				}
				records, err := file.Load(ctx)
				if err != nil {
					t.Fatal(err)
				}
				foundFable, foundOther := false, false
				for _, record := range records {
					if record.AuthID == id && record.Quota.Reason == "credential_quota" {
						t.Error("recovered shared exhaustion remained on disk")
					}
					if record.AuthID == id && record.Model == fable && record.NextRetryAfter.Equal(fableDeadline) {
						foundFable = true
					}
					if record.AuthID == otherID && record.Model == base {
						foundOther = true
					}
				}
				if foundFable == fableRecovered || !foundOther {
					t.Errorf("reset deleted unaffected disk records: Fable=%t other-auth=%t", foundFable, foundOther)
				}
			})
		}
	}
}

func TestQuotaResetBeforeFirstRestoreClearsOldDiskQuota(t *testing.T) {
	ctx := context.Background()
	const id = "quota-reset-first"
	donor := NewManager(nil, nil, nil)
	if _, err := donor.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := donor.ApplyProviderQuotaReport(ctx, id, true, time.Now().Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	file := NewFileCooldownStateStore(t.TempDir())
	if err := file.Save(ctx, donor.cooldownStateRecordsSnapshot()); err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil, nil, nil)
	if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	m.SetCooldownStateStore(file)
	if _, err := m.ApplyProviderQuotaReset(ctx, id, nil, nil); err != nil {
		t.Fatal(err)
	}
	if records, err := file.Load(ctx); err != nil || len(records) != 0 {
		t.Errorf("reset did not reconcile old disk quota before returning: records=%d err=%v", len(records), err)
	}
	if err := m.RestoreCooldownStates(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetByID(id)
	if blocked, _, _ := isAuthBlockedForModel(got, "claude-opus-5", time.Now()); blocked {
		t.Fatal("reset before first restore reintroduced persisted exhaustion")
	}
}

func TestQuotaResetBeforeRestorePreservesNotYetRegisteredAuth(t *testing.T) {
	ctx := context.Background()
	const id, pendingID = "quota-reset-registered", "quota-register-later"
	donor := NewManager(nil, nil, nil)
	for _, authID := range []string{id, pendingID} {
		if _, err := donor.Register(ctx, &Auth{ID: authID, Provider: "claude"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := donor.ApplyProviderQuotaReport(ctx, id, true, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	retry := 3 * time.Hour
	donor.MarkResult(ctx, Result{AuthID: pendingID, Model: "fable", RetryAfter: &retry, Error: &Error{Code: ErrorCodeForceCooldown, HTTPStatus: 503}})
	file := NewFileCooldownStateStore(t.TempDir())
	if err := file.Save(ctx, donor.cooldownStateRecordsSnapshot()); err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil, nil, nil)
	if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	m.SetCooldownStateStore(file)
	if _, err := m.ApplyProviderQuotaReset(ctx, id, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Register(ctx, &Auth{ID: pendingID, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	if err := m.RestoreCooldownStates(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetByID(pendingID)
	if blocked, _, _ := isAuthBlockedForModel(got, "fable", time.Now()); !blocked {
		t.Fatal("reset deleted cooldown records for an auth awaiting registration")
	}
	// Once that credential has actually been registered and removed, its old
	// unread record must no longer survive a snapshot write.
	removing := NewManager(nil, nil, nil)
	removing.SetCooldownStateStore(file)
	if err := removing.RestoreCooldownStates(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := removing.Register(ctx, &Auth{ID: pendingID, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	removing.Remove(ctx, pendingID)
	removing.PersistCooldownStates(ctx)
	if records, err := file.Load(ctx); err != nil || len(records) != 0 {
		t.Fatalf("unread records resurrected a removed auth: records=%d err=%v", len(records), err)
	}
}
