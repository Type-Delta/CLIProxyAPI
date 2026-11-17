package auth

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestQuotaLiveTransitionNoHistoryKeepsScopedQuota(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	const id = "quota-empty-shared-scoped"
	if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := m.ApplyProviderQuotaReport(ctx, id, true, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	scopedDeadline := now.Add(3 * time.Hour)
	if _, err := m.ApplyProviderQuotaReportForModels(ctx, id, true, scopedDeadline, []string{"fable"}); err != nil {
		t.Fatal(err)
	}
	retry := 15 * time.Minute
	m.MarkResult(ctx, Result{AuthID: id, Model: "fable", RetryAfter: &retry, Error: &Error{HTTPStatus: 503}})
	if _, err := m.ApplyProviderQuotaReport(ctx, id, false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetByID(id)
	if blocked, _, next := isAuthBlockedForModel(got, "fable", now.Add(2*time.Hour)); !blocked || !next.Equal(scopedDeadline) {
		t.Fatalf("shared-only recovery lost scoped quota: blocked=%t next=%v want=%v state=%+v", blocked, next, scopedDeadline, got.ModelStates["fable"])
	}
	if _, err := m.ApplyProviderQuotaReportForModels(ctx, id, false, time.Time{}, []string{"fable"}); err != nil {
		t.Fatal(err)
	}
	got, _ = m.GetByID(id)
	if state := got.ModelStates["fable"]; state.LastError == nil || state.LastError.HTTPStatus != 503 || !state.NextRetryAfter.After(now) || state.NextRetryAfter.After(now.Add(time.Hour)) {
		t.Fatalf("scoped recovery lost independent failure: %+v", state)
	}
}

type gatedQuotaFilePublication struct {
	*FileCooldownStateStore
	saving      chan struct{}
	releaseSave chan struct{}
	loaded      chan struct{}
	releaseLoad chan struct{}
	firstSave   atomic.Bool
}

func (s *gatedQuotaFilePublication) Save(ctx context.Context, records []CooldownStateRecord) error {
	if s.firstSave.CompareAndSwap(false, true) {
		close(s.saving)
		<-s.releaseSave
	}
	return s.FileCooldownStateStore.Save(ctx, records)
}

func (s *gatedQuotaFilePublication) Load(ctx context.Context) ([]CooldownStateRecord, error) {
	records, err := s.FileCooldownStateStore.Load(ctx)
	if s.firstSave.Load() {
		close(s.loaded)
		<-s.releaseLoad
	}
	return records, err
}

func TestQuotaRestoreStartedDuringResetPublicationWaitsForPersistence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		m := NewManager(nil, nil, nil)
		const id = "reset-publication"
		if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
			t.Fatal(err)
		}
		if _, err := m.ApplyProviderQuotaReport(ctx, id, true, time.Now().Add(3*time.Hour)); err != nil {
			t.Fatal(err)
		}
		file := NewFileCooldownStateStore(t.TempDir())
		if err := file.Save(ctx, m.cooldownStateRecordsSnapshot()); err != nil {
			t.Fatal(err)
		}
		store := &gatedQuotaFilePublication{FileCooldownStateStore: file, saving: make(chan struct{}), releaseSave: make(chan struct{}), loaded: make(chan struct{}), releaseLoad: make(chan struct{})}
		m.SetCooldownStateStore(store)
		resetDone := make(chan error, 1)
		go func() {
			_, err := m.ApplyProviderQuotaReset(ctx, id, nil, nil)
			resetDone <- err
		}()
		<-store.saving
		if m.QuotaObservationEpoch(id) == 0 {
			t.Error("reset has not published its new observation epoch")
		}
		restoreDone := make(chan error, 1)
		go func() { restoreDone <- m.RestoreCooldownStates(ctx) }()
		synctest.Wait()
		select {
		case <-store.loaded:
			t.Error("restore read old disk records while reset persistence was pending")
		default:
		}
		close(store.releaseSave)
		if err := <-resetDone; err != nil {
			t.Error(err)
		}
		close(store.releaseLoad)
		if err := <-restoreDone; err != nil {
			t.Fatal(err)
		}
		got, _ := m.GetByID(id)
		if blocked, _, next := isAuthBlockedForModel(got, "fable", time.Now()); blocked {
			t.Fatalf("restore with new epoch reinstated old disk quota: next=%v quota=%+v", next, got.Quota)
		}
	})
}

func TestQuotaResetRejectsOnlyRecoveredPreResetQuotaFailures(t *testing.T) {
	for _, tc := range []struct {
		name            string
		provider        string
		model           string
		credentialScope bool
		resetAll        bool
		recoverShared   bool
		newSequence     bool
		legacySequence  bool
		emptySelection  bool
		code            string
		status          int
		wantBlocked     bool
	}{
		{name: "old shared quota", model: "opus", credentialScope: true, resetAll: true, recoverShared: true, status: 429},
		{name: "old selected quota", model: "opus", recoverShared: true, status: 429},
		{name: "old shared quota after selected base reset", model: "opus", credentialScope: true, recoverShared: true, status: 429},
		{name: "old unselected Fable quota", model: "fable", recoverShared: true, status: 429, wantBlocked: true},
		{name: "old shared quota after model-only reset", model: "opus", credentialScope: true, status: 429, wantBlocked: true},
		{name: "new shared quota", model: "opus", credentialScope: true, resetAll: true, recoverShared: true, newSequence: true, status: 429, wantBlocked: true},
		{name: "legacy shared quota", model: "opus", credentialScope: true, resetAll: true, recoverShared: true, legacySequence: true, status: 429, wantBlocked: true},
		{name: "old outage", model: "opus", resetAll: true, recoverShared: true, status: 503, wantBlocked: true},
		{name: "old capacity", model: "opus", resetAll: true, recoverShared: true, code: ErrorCodeTransientRateLimit, status: 429, wantBlocked: true},
		{name: "old forced quota", model: "opus", resetAll: true, recoverShared: true, code: ErrorCodeForceCooldown, status: 429, wantBlocked: true},
		{name: "old unselected Spark quota", provider: "codex", model: "fable", recoverShared: true, status: 429, wantBlocked: true},
		{name: "old selected Codex base quota", provider: "codex", model: "opus", recoverShared: true, status: 429},
		{name: "old quota after unknown reset scope", model: "opus", emptySelection: true, status: 429, wantBlocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			hook := &resultCaptureHook{}
			m := NewManager(nil, nil, hook)
			const id = "reset-inflight"
			provider, base, family := "claude", "claude-opus-5", "claude-fable-5-2"
			if tc.provider == "codex" {
				provider, base, family = "codex", "gpt-5.5", "gpt-5.3-codex-spark"
			}
			model := base
			if tc.model == "fable" {
				model = family
			}
			auth, err := m.Register(ctx, &Auth{ID: id, Provider: provider, ModelStates: map[string]*ModelState{base: {Status: StatusActive}, family: {Status: StatusActive}}})
			if err != nil {
				t.Fatal(err)
			}
			oldSeq, currentSeq := m.nextExecutionSeq(), m.nextExecutionSeq()
			retry := 3 * time.Hour
			m.MarkResult(ctx, Result{AuthID: id, Model: base, CredentialScope: true, ExecutionSeq: currentSeq, CredentialVersion: auth.CredentialVersion, RetryAfter: &retry, Error: &Error{HTTPStatus: 429}})
			models := []string{base}
			if tc.resetAll {
				models = nil
			} else if tc.emptySelection {
				models = []string{}
			}
			if tc.recoverShared {
				_, err = m.ApplyProviderQuotaReset(ctx, id, models, nil)
			} else {
				_, err = m.ApplyProviderQuotaResetForModels(ctx, id, models, nil)
				// Remove only this fixture's original shared block without a reset
				// fence so the following error reveals the selected reset's scope.
				if err == nil {
					_, err = m.ApplyProviderQuotaReport(ctx, id, false, time.Time{})
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before, _ := m.GetByID(id)
			if blocked, _, _ := isAuthBlockedForModel(before, model, time.Now()); blocked {
				t.Fatal("reset fixture remained unavailable")
			}
			seq := oldSeq
			if tc.newSequence {
				seq = m.nextExecutionSeq()
			} else if tc.legacySequence {
				seq = 0
			}
			m.MarkResult(ctx, Result{AuthID: id, Model: model, CredentialScope: tc.credentialScope, ExecutionSeq: seq, CredentialVersion: auth.CredentialVersion, RetryAfter: &retry, Error: &Error{HTTPStatus: tc.status, Code: tc.code}})
			got, _ := m.GetByID(id)
			if blocked, _, _ := isAuthBlockedForModel(got, model, time.Now()); blocked != tc.wantBlocked {
				t.Errorf("pre-reset result %s: blocked=%t want=%t", tc.name, blocked, tc.wantBlocked)
			}
			if got.Failed != before.Failed+1 {
				t.Error("old request failure disappeared from accounting")
			}
			results := hook.Results()
			if len(results) != 2 || results[1].Success || results[1].ExecutionSeq != seq || results[1].Error == nil || results[1].Error.HTTPStatus != tc.status {
				t.Fatalf("old request failure disappeared from result hook: %+v", results)
			}
		})
	}
}

func TestQuotaLiveTransitionScopedRecoveryKeepsIndependentForce(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	const id = "quota-scoped-recovery-force"
	if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	retry := 3 * time.Hour
	m.MarkResult(ctx, Result{AuthID: id, Model: "fable", RetryAfter: &retry, Error: &Error{Code: ErrorCodeForceCooldown, HTTPStatus: 503}})
	before, _ := m.GetByID(id)
	deadline := before.ModelStates["fable"].NextRetryAfter
	if _, err := m.ApplyProviderQuotaReport(ctx, id, true, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApplyProviderQuotaReportForModels(ctx, id, false, time.Time{}, []string{"fable"}); err != nil {
		t.Fatal(err)
	}
	retry = 15 * time.Minute
	m.MarkResult(ctx, Result{AuthID: id, Model: "fable", RetryAfter: &retry, Error: &Error{HTTPStatus: 503}})
	m.SetConfig(&internalconfig.Config{DisableCooling: true})
	got, _ := m.GetByID(id)
	if blocked, _, next := isAuthBlockedForModel(got, "fable", time.Now()); !blocked || !next.Equal(deadline) {
		t.Fatalf("live result lost independent force after scoped recovery: blocked=%t next=%v want=%v state=%+v", blocked, next, deadline, got.ModelStates["fable"])
	}
}

type gatedQuotaFileLoad struct {
	*FileCooldownStateStore
	loaded    chan struct{}
	release   chan struct{}
	firstLoad atomic.Bool
}

func (s *gatedQuotaFileLoad) Load(ctx context.Context) ([]CooldownStateRecord, error) {
	records, err := s.FileCooldownStateStore.Load(ctx)
	if err != nil {
		return nil, err
	}
	if !s.firstLoad.CompareAndSwap(false, true) {
		return records, nil
	}
	close(s.loaded)
	select {
	case <-s.release:
		return records, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestQuotaRestoreStartedBeforeResetCannotReinstateQuota(t *testing.T) {
	ctx := context.Background()
	donor := NewManager(nil, nil, nil)
	const resetID, forceID, ordinaryID = "reset-quota", "other-force", "other-ordinary"
	for _, id := range []string{resetID, forceID, ordinaryID} {
		if _, err := donor.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := donor.ApplyProviderQuotaReport(ctx, resetID, true, time.Now().Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	retry := 2 * time.Hour
	donor.MarkResult(ctx, Result{AuthID: forceID, Model: "fable", RetryAfter: &retry, Error: &Error{Code: ErrorCodeForceCooldown, HTTPStatus: 503}})
	donor.MarkResult(ctx, Result{AuthID: ordinaryID, Model: "fable", RetryAfter: &retry, Error: &Error{HTTPStatus: 503}})
	file := NewFileCooldownStateStore(t.TempDir())
	if err := file.Save(ctx, donor.cooldownStateRecordsSnapshot()); err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil, nil, nil)
	for _, id := range []string{resetID, forceID, ordinaryID} {
		if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.ApplyProviderQuotaReport(ctx, resetID, true, time.Now().Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	store := &gatedQuotaFileLoad{FileCooldownStateStore: file, loaded: make(chan struct{}), release: make(chan struct{})}
	m.SetCooldownStateStore(store)
	done := make(chan error, 1)
	go func() { done <- m.RestoreCooldownStates(ctx) }()
	<-store.loaded
	if _, err := m.ApplyProviderQuotaReset(ctx, resetID, nil, nil); err != nil {
		close(store.release)
		<-done
		t.Fatal(err)
	}
	before, _ := m.GetByID(resetID)
	if blocked, _, _ := isAuthBlockedForModel(before, "fable", time.Now()); blocked {
		close(store.release)
		<-done
		t.Fatal("reset did not make credential available before delayed restore")
	}
	// A new failure after reset is legitimate data and must remain persisted.
	retry = 4 * time.Hour
	m.MarkResult(ctx, Result{AuthID: resetID, Model: "opus", RetryAfter: &retry, Error: &Error{HTTPStatus: 503}})
	newer, _ := m.GetByID(resetID)
	newDeadline := newer.ModelStates["opus"].NextRetryAfter
	close(store.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetByID(resetID)
	if blocked, _, next := isAuthBlockedForModel(got, "fable", time.Now()); blocked {
		t.Errorf("delayed restore reinstated reset quota: next=%v quota=%+v", next, got.Quota)
	}
	if state := got.ModelStates["opus"]; state == nil || !state.NextRetryAfter.Equal(newDeadline) {
		t.Errorf("delayed restore lost post-reset failure: %+v", state)
	}
	for _, id := range []string{forceID, ordinaryID} {
		auth, _ := m.GetByID(id)
		if blocked, _, _ := isAuthBlockedForModel(auth, "fable", time.Now()); !blocked {
			t.Errorf("reset on another auth discarded legitimate restore for %s", id)
		}
	}
	records, err := file.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundNew := false
	for _, record := range records {
		if record.AuthID != resetID {
			continue
		}
		if record.Quota.Reason == "credential_quota" {
			t.Errorf("delayed restore repersisted pre-reset quota: %+v", record)
		}
		if record.Model == "opus" && record.NextRetryAfter.Equal(newDeadline) {
			foundNew = true
		}
	}
	if !foundNew {
		t.Fatal("post-reset failure missing from file store")
	}
}

func TestQuotaCapacityRecoveryAfterFileRestore(t *testing.T) {
	for _, capacityRetry := range []time.Duration{0, 15 * time.Minute} {
		for _, liveQuota := range []bool{false, true} {
			t.Run(capacityRetry.String()+"/live-quota="+strconv.FormatBool(liveQuota), func(t *testing.T) {
				ctx := context.Background()
				const id = "quota-capacity-file"
				donor := NewManager(nil, nil, nil)
				if _, err := donor.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
					t.Fatal(err)
				}
				retry := 3 * time.Hour
				donor.MarkResult(ctx, Result{AuthID: id, Model: "fable", RetryAfter: &retry, Error: &Error{HTTPStatus: 429}})
				before := time.Now()
				donor.MarkResult(ctx, Result{AuthID: id, Model: "fable", RetryAfter: &capacityRetry, Error: &Error{Code: ErrorCodeTransientRateLimit, HTTPStatus: 429}})
				after := time.Now()
				// A later genuine quota refusal updates the quota layer without
				// turning it into an independent capacity deadline.
				donor.MarkResult(ctx, Result{AuthID: id, Model: "fable", RetryAfter: &retry, Error: &Error{HTTPStatus: 429}})
				file := NewFileCooldownStateStore(t.TempDir())
				if err := file.Save(ctx, donor.cooldownStateRecordsSnapshot()); err != nil {
					t.Fatal(err)
				}
				m := NewManager(nil, nil, nil)
				if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
					t.Fatal(err)
				}
				if liveQuota {
					m.MarkResult(ctx, Result{AuthID: id, Model: "fable", RetryAfter: &retry, Error: &Error{HTTPStatus: 429}})
				}
				m.SetCooldownStateStore(file)
				if err := m.RestoreCooldownStates(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := m.ApplyProviderQuotaRecovery(ctx, id, []string{"fable"}); err != nil {
					t.Fatal(err)
				}
				got, _ := m.GetByID(id)
				state := got.ModelStates["fable"]
				if state.Quota.Exceeded || !state.Quota.NextRecoverAt.IsZero() {
					t.Errorf("restored quota was not recovered: %+v", state)
				}
				if capacityRetry > 0 && (state.NextRetryAfter.Before(before.Add(capacityRetry)) || state.NextRetryAfter.After(after.Add(capacityRetry))) {
					t.Errorf("restored capacity deadline=%v, want independent %s retry", state.NextRetryAfter, capacityRetry)
				}
				if blocked, _, _ := isAuthBlockedForModel(got, "fable", time.Now()); blocked != (capacityRetry > 0) {
					t.Errorf("restored capacity=%s blocked=%t", capacityRetry, blocked)
				}
			})
		}
	}
}

func TestQuotaCapacityZeroDoesNotLoseExplicitForce(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	const id = "quota-capacity-force"
	if _, err := m.Register(ctx, &Auth{ID: id, Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	retry := 3 * time.Hour
	m.MarkResult(ctx, Result{AuthID: id, Model: "fable", RetryAfter: &retry, Error: &Error{HTTPStatus: 429, Code: ErrorCodeForceCooldown}})
	before, _ := m.GetByID(id)
	deadline := before.ModelStates["fable"].NextRetryAfter
	retry = 0
	m.MarkResult(ctx, Result{AuthID: id, Model: "fable", RetryAfter: &retry, Error: &Error{HTTPStatus: 429, Code: ErrorCodeTransientRateLimit}})
	if _, err := m.ApplyProviderQuotaRecovery(ctx, id, []string{"fable"}); err != nil {
		t.Fatal(err)
	}
	m.SetConfig(&internalconfig.Config{DisableCooling: true})
	got, _ := m.GetByID(id)
	if blocked, _, next := isAuthBlockedForModel(got, "fable", time.Now()); !blocked || !next.Equal(deadline) {
		t.Fatalf("quota/capacity recovery lost explicit force: blocked=%t next=%v want=%v", blocked, next, deadline)
	}
}
