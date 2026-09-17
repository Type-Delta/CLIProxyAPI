package auth

import (
	"context"
	"testing"
	"time"
)

func TestManager_ApplyProviderQuotaReport_ExhaustedOverwritesCooldown(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	model := "quota-report-model"
	oldReset := time.Now().Add(2 * time.Hour)
	fetchedReset := time.Now().Add(91 * time.Hour).Round(time.Second)

	if _, errRegister := manager.Register(ctx, &Auth{
		ID:       "quota-report-auth",
		Provider: "openai-compatible-opencode-go",
		Status:   StatusError,
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: oldReset,
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: oldReset},
			},
		},
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	changed, errReport := manager.ApplyProviderQuotaReport(ctx, "quota-report-auth", true, fetchedReset)
	if errReport != nil {
		t.Fatalf("ApplyProviderQuotaReport() error = %v", errReport)
	}
	if !changed {
		t.Fatal("ApplyProviderQuotaReport() changed = false, want true")
	}

	updated, ok := manager.GetByID("quota-report-auth")
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatal("updated model state missing")
	}
	if !state.Unavailable || state.Status != StatusError || !state.NextRetryAfter.Equal(fetchedReset) {
		t.Fatalf("model cooldown = unavailable %v status %q retry %v, want retry %v", state.Unavailable, state.Status, state.NextRetryAfter, fetchedReset)
	}
	if !state.Quota.Exceeded || state.Quota.Reason != "quota" || !state.Quota.NextRecoverAt.Equal(fetchedReset) {
		t.Fatalf("model quota = %+v, want recovery at %v", state.Quota, fetchedReset)
	}
}

func TestManager_ApplyProviderQuotaReport_HealthyClearsQuotaCooldown(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	model := "quota-report-model"
	reset := time.Now().Add(91 * time.Hour)

	if _, errRegister := manager.Register(ctx, &Auth{
		ID:             "quota-report-auth",
		Provider:       "openai-compatible-opencode-go",
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: reset,
		Quota:          QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: reset},
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				StatusMessage:  "weekly usage limit reached",
				Unavailable:    true,
				NextRetryAfter: reset,
				LastError:      &Error{Code: "GoUsageLimitError"},
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: reset},
			},
		},
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	changed, errReport := manager.ApplyProviderQuotaReport(ctx, "quota-report-auth", false, time.Time{})
	if errReport != nil {
		t.Fatalf("ApplyProviderQuotaReport() error = %v", errReport)
	}
	if !changed {
		t.Fatal("ApplyProviderQuotaReport() changed = false, want true")
	}

	updated, ok := manager.GetByID("quota-report-auth")
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.Status != StatusActive || updated.Unavailable || !updated.NextRetryAfter.IsZero() || updated.Quota.Exceeded {
		t.Fatalf("auth state = status %q unavailable %v retry %v quota %+v", updated.Status, updated.Unavailable, updated.NextRetryAfter, updated.Quota)
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatal("updated model state missing")
	}
	if state.Status != StatusActive || state.Unavailable || !state.NextRetryAfter.IsZero() || state.LastError != nil || state.Quota.Exceeded {
		t.Fatalf("model state = status %q unavailable %v retry %v error %+v quota %+v", state.Status, state.Unavailable, state.NextRetryAfter, state.LastError, state.Quota)
	}
}

func TestManager_ApplyProviderQuotaReport_IgnoresPastReset(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	model := "quota-report-model"
	reset := time.Now().Add(time.Hour)
	auth := &Auth{
		ID:       "quota-report-auth",
		Provider: "openai-compatible-opencode-go",
		ModelStates: map[string]*ModelState{
			model: {
				NextRetryAfter: reset,
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: reset},
			},
		},
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	before, _ := manager.GetByID(auth.ID)
	beforeGeneration := before.Generation

	changed, errReport := manager.ApplyProviderQuotaReport(ctx, auth.ID, true, time.Now().Add(-time.Minute))
	if errReport != nil {
		t.Fatalf("ApplyProviderQuotaReport() error = %v", errReport)
	}
	if changed {
		t.Fatal("ApplyProviderQuotaReport() changed = true, want false")
	}
	after, _ := manager.GetByID(auth.ID)
	if after.Generation != beforeGeneration {
		t.Fatalf("generation = %d, want unchanged %d", after.Generation, beforeGeneration)
	}
	if !after.ModelStates[model].NextRetryAfter.Equal(reset) {
		t.Fatalf("retry after = %v, want %v", after.ModelStates[model].NextRetryAfter, reset)
	}
}
