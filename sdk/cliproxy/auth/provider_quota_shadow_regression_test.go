package auth

import (
	"context"
	"testing"
	"time"
)

func TestProviderQuotaReportPreservesIndependentDeadline(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	independent := now.Add(3 * time.Hour)
	m := NewManager(nil, nil, nil)
	_, err := m.Register(ctx, &Auth{ID: "quota-shadow-deadline", Provider: "claude", ModelStates: map[string]*ModelState{
		"opus": {Status: StatusError, Unavailable: true, NextRetryAfter: independent, LastError: &Error{HTTPStatus: 503}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.ApplyProviderQuotaReport(ctx, "quota-shadow-deadline", true, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetByID("quota-shadow-deadline")
	if blocked, _, _ := isAuthBlockedForModel(got, "opus", now.Add(2*time.Hour)); !blocked {
		t.Fatal("independent 503 must still block after shared quota expires")
	}
	if _, err = m.ApplyProviderQuotaReport(ctx, "quota-shadow-deadline", false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	got, _ = m.GetByID("quota-shadow-deadline")
	if state := got.ModelStates["opus"]; state.LastError == nil || state.LastError.HTTPStatus != 503 || !state.NextRetryAfter.Equal(independent) {
		t.Fatalf("independent state lost: %+v", state)
	}
}

func TestProviderQuotaScopedRecoveryPreservesIndependentShadow(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	independent := now.Add(3 * time.Hour)
	m := NewManager(nil, nil, nil)
	_, err := m.Register(ctx, &Auth{ID: "quota-shadow-recovery", Provider: "claude", ModelStates: map[string]*ModelState{
		"fable": {Status: StatusError, Unavailable: true, NextRetryAfter: independent, LastError: &Error{HTTPStatus: 503}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.ApplyProviderQuotaReport(ctx, "quota-shadow-recovery", true, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = m.ApplyProviderQuotaReportForModels(ctx, "quota-shadow-recovery", false, time.Time{}, []string{"fable"}); err != nil {
		t.Fatal(err)
	}
	if _, err = m.ApplyProviderQuotaReport(ctx, "quota-shadow-recovery", false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetByID("quota-shadow-recovery")
	if state := got.ModelStates["fable"]; state.LastError == nil || state.LastError.HTTPStatus != 503 || !state.NextRetryAfter.Equal(independent) {
		t.Fatalf("independent state lost: %+v", state)
	}
}

func TestProviderQuotaResetPreservesDisabledAndIndependentFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	independent := now.Add(3 * time.Hour)
	m := NewManager(nil, nil, nil)
	_, err := m.Register(ctx, &Auth{ID: "quota-reset-preserves-models", Provider: "claude", ModelStates: map[string]*ModelState{
		"disabled": {Status: StatusDisabled},
		"fable":    {Status: StatusError, Unavailable: true, NextRetryAfter: independent, LastError: &Error{HTTPStatus: 503}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.ApplyProviderQuotaReport(ctx, "quota-reset-preserves-models", true, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = m.ApplyProviderQuotaReset(ctx, "quota-reset-preserves-models", nil, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetByID("quota-reset-preserves-models")
	if state := got.ModelStates["disabled"]; state.Status != StatusDisabled {
		t.Fatalf("disabled model changed: %+v", state)
	}
	if state := got.ModelStates["fable"]; state.LastError == nil || state.LastError.HTTPStatus != 503 || !state.NextRetryAfter.Equal(independent) {
		t.Fatalf("independent state lost: %+v", state)
	}
}

func TestProviderResetThenCooldownResetPreservesDisabledModel(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	m := NewManager(nil, nil, nil)
	_, err := m.Register(ctx, &Auth{ID: "panel-reset-disabled", Provider: "claude", ModelStates: map[string]*ModelState{
		"disabled": {Status: StatusDisabled, StatusMessage: "disabled", Unavailable: true, NextRetryAfter: now.Add(3 * time.Hour)},
		"fable":    {Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour), Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(time.Hour)}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.ApplyProviderQuotaReset(ctx, "panel-reset-disabled", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err = m.ResetQuota(ctx, "panel-reset-disabled"); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetByID("panel-reset-disabled")
	if state := got.ModelStates["disabled"]; state.Status != StatusDisabled || state.StatusMessage != "disabled" || !state.Unavailable || !state.NextRetryAfter.Equal(now.Add(3*time.Hour)) {
		t.Fatalf("disabled model changed: %+v", state)
	}
	if blocked, reason, _ := isAuthBlockedForModel(got, "disabled", now); !blocked || reason != blockReasonDisabled {
		t.Fatalf("disabled model routing: blocked=%v reason=%v", blocked, reason)
	}
	if blocked, _, _ := isAuthBlockedForModel(got, "fable", now); blocked {
		t.Fatal("reset quota should allow healthy model")
	}
}

func TestScopedQuotaIndependentLayerPersistence(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "scoped", true: "shared_over_scoped"}[nested], func(t *testing.T) {
			ctx := context.Background()
			now := time.Now()
			deadline := now.Add(3 * time.Hour)
			m := NewManager(nil, nil, nil)
			_, err := m.Register(ctx, &Auth{ID: "scoped-layer", Provider: "claude", ModelStates: map[string]*ModelState{"fable": {Status: StatusError, Unavailable: true, NextRetryAfter: deadline, LastError: &Error{HTTPStatus: 503}}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.ApplyProviderQuotaReportForModels(ctx, "scoped-layer", true, now.Add(time.Hour), []string{"fable"}); err != nil {
				t.Fatal(err)
			}
			if nested {
				if _, err = m.ApplyProviderQuotaReport(ctx, "scoped-layer", true, now.Add(90*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			got, _ := m.GetByID("scoped-layer")
			if blocked, _, _ := isAuthBlockedForModel(got, "fable", now.Add(2*time.Hour)); !blocked {
				t.Fatal("independent 503 lost at two hours")
			}
			m.mu.RLock()
			records := m.cooldownStateRecordsForAuthLocked(m.auths["scoped-layer"], now)
			m.mu.RUnlock()
			store := NewFileCooldownStateStore(t.TempDir())
			if err = store.Save(ctx, records); err != nil {
				t.Fatal(err)
			}
			records, err = store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			m2 := NewManager(nil, nil, nil)
			if _, err = m2.Register(ctx, &Auth{ID: "scoped-layer", Provider: "claude"}); err != nil {
				t.Fatal(err)
			}
			m2.mu.Lock()
			for _, r := range records {
				m2.restoreCooldownRecordLocked(r, now)
			}
			m2.mu.Unlock()
			if nested {
				if _, err = m2.ApplyProviderQuotaReport(ctx, "scoped-layer", false, time.Time{}); err != nil {
					t.Fatal(err)
				}
				got, _ = m2.GetByID("scoped-layer")
				if state := got.ModelStates["fable"]; !state.Quota.Exceeded {
					t.Fatalf("shared recovery lost scoped quota: %+v", state)
				}
			}
			if _, err = m2.ApplyProviderQuotaReportForModels(ctx, "scoped-layer", false, time.Time{}, []string{"fable"}); err != nil {
				t.Fatal(err)
			}
			got, _ = m2.GetByID("scoped-layer")
			if state := got.ModelStates["fable"]; state.LastError == nil || state.LastError.HTTPStatus != 503 || !state.NextRetryAfter.Equal(deadline) {
				t.Fatalf("scoped recovery lost 503: %+v", state)
			}
		})
	}
}

func TestScopedQuotaWithinSharedWrap(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	deadline := now.Add(3 * time.Hour)
	m := NewManager(nil, nil, nil)
	_, err := m.Register(ctx, &Auth{ID: "scoped-within-shared", Provider: "claude", ModelStates: map[string]*ModelState{"fable": {Status: StatusError, Unavailable: true, NextRetryAfter: deadline, LastError: &Error{HTTPStatus: 503}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.ApplyProviderQuotaReport(ctx, "scoped-within-shared", true, now.Add(90*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = m.ApplyProviderQuotaReportForModels(ctx, "scoped-within-shared", true, now.Add(time.Hour), []string{"fable"}); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetByID("scoped-within-shared")
	if blocked, _, _ := isAuthBlockedForModel(got, "fable", now.Add(2*time.Hour)); !blocked {
		t.Fatal("independent 503 lost")
	}
	if _, err = m.ApplyProviderQuotaReportForModels(ctx, "scoped-within-shared", false, time.Time{}, []string{"fable"}); err != nil {
		t.Fatal(err)
	}
	got, _ = m.GetByID("scoped-within-shared")
	if !got.Quota.Exceeded || got.Quota.Reason != "credential_quota" {
		t.Fatal("scoped recovery lost shared quota")
	}
	if _, err = m.ApplyProviderQuotaReport(ctx, "scoped-within-shared", false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	got, _ = m.GetByID("scoped-within-shared")
	if state := got.ModelStates["fable"]; state.LastError == nil || state.LastError.HTTPStatus != 503 || !state.NextRetryAfter.Equal(deadline) {
		t.Fatalf("independent 503 lost: %+v", state)
	}
}

func TestScopedQuotaPreservesNewIndependentFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	m := NewManager(nil, nil, nil)
	if _, err := m.Register(ctx, &Auth{ID: "scoped-new-failure", Provider: "claude", ModelStates: map[string]*ModelState{"fable": {Status: StatusActive}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApplyProviderQuotaReportForModels(ctx, "scoped-new-failure", true, now.Add(time.Hour), []string{"fable"}); err != nil {
		t.Fatal(err)
	}
	retry := 3 * time.Hour
	m.MarkResult(ctx, Result{AuthID: "scoped-new-failure", Provider: "claude", Model: "fable", Error: &Error{HTTPStatus: 503}, RetryAfter: &retry})
	if _, err := m.ApplyProviderQuotaReportForModels(ctx, "scoped-new-failure", false, time.Time{}, []string{"fable"}); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetByID("scoped-new-failure")
	if state := got.ModelStates["fable"]; state.LastError == nil || state.LastError.HTTPStatus != 503 || !state.NextRetryAfter.After(now.Add(2*time.Hour)) {
		t.Fatalf("new failure lost: %+v", state)
	}
}
