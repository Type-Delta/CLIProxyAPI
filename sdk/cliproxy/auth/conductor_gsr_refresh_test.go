package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type quotaRecoveryTestSelector struct{}

func TestUpdateQuotaObservationPreservesCredentialCooldown(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	reset := time.Now().Add(time.Hour)
	auth, errRegister := manager.Register(context.Background(), &Auth{
		ID: "observed-cooldown", Provider: "codex", Unavailable: true,
		NextRetryAfter: reset,
		Quota:          QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: reset},
	})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	observation := QuotaState{ObservedAt: time.Now(), Signals: map[string]string{"X-Codex-Primary-Reset-At": strconvFormatInt(reset.Unix())}}
	if _, errUpdate := manager.UpdateQuotaObservation(context.Background(), auth.ID, observation); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated.Quota.Signals["X-Codex-Primary-Reset-At"] != observation.Signals["X-Codex-Primary-Reset-At"] {
		t.Fatal("quota observation was lost")
	}
	if !updated.Unavailable || !updated.Quota.Exceeded || !updated.Quota.NextRecoverAt.Equal(reset) || !updated.NextRetryAfter.Equal(reset) {
		t.Fatal("quota observation changed the credential cooldown")
	}
}

func (quotaRecoveryTestSelector) RequiresQuotaRecoveryObservation() bool { return true }
func (quotaRecoveryTestSelector) Pick(_ context.Context, _ string, _ string, _ cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if len(auths) == 0 {
		return nil, errors.New("no candidates")
	}
	return auths[0], nil
}

func TestRecoveryTimestampForAuthProviderWindows(t *testing.T) {
	reset := time.Now().Add(24 * time.Hour).UTC()
	resetUnix := strconvFormatInt(reset.Unix())
	tests := []struct {
		name     string
		provider string
		model    string
		signals  map[string]string
		want     time.Time
	}{
		{
			name:     "fable prefers oi window",
			provider: "claude",
			model:    "claude-fable-5-1",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Reset":    strconvFormatInt(reset.Add(2 * time.Hour).Unix()),
				"Anthropic-Ratelimit-Unified-7d_oi-Reset": resetUnix,
			},
			want: reset,
		},
		{
			name:     "fable falls back to seven day",
			provider: "claude",
			model:    "claude-fable-5-1",
			signals:  map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": resetUnix},
			want:     reset,
		},
		{
			name:     "codex weekly reset",
			provider: "codex",
			model:    "gpt-5.5",
			signals:  map[string]string{"X-Codex-Primary-Reset-At": resetUnix},
			want:     reset,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := RecoveryTimestampForAuth(&Auth{Provider: test.provider, Quota: QuotaState{Signals: test.signals}}, test.model)
			if got.Unix() != test.want.Unix() {
				t.Fatalf("recovery timestamp = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRecoveryTimestampForAuthProbeCadenceFallback(t *testing.T) {
	reset := time.Now().Add(48 * time.Hour).UTC()
	for _, test := range []struct {
		provider string
		model    string
	}{
		{provider: "openai-compatible-opencode-go", model: "gpt-5"},
		{provider: "zai", model: "glm-5"},
	} {
		auth := &Auth{Provider: test.provider, Quota: QuotaState{NextRecoverAt: reset}}
		if got := RecoveryTimestampForAuth(auth, test.model); got.Unix() != reset.Unix() {
			t.Fatalf("%s/%s recovery timestamp = %v, want %v", test.provider, test.model, got, reset)
		}
	}
}

func TestPrepareQuotaRecoveryCandidatesRefreshesMissingAndReusesCache(t *testing.T) {
	manager := NewManager(nil, quotaRecoveryTestSelector{}, nil)
	cachedReset := time.Now().Add(12 * time.Hour)
	for _, auth := range []*Auth{
		{ID: "cached", Provider: "codex", Quota: QuotaState{NextRecoverAt: cachedReset, ManualReset: ManualResetObservation{Provider: "codex", ObservedAt: time.Now()}}},
		{ID: "missing", Provider: "codex"},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s): %v", auth.ID, errRegister)
		}
	}
	var refreshed []string
	manager.SetQuotaRecoveryRefresher(func(_ context.Context, auth *Auth, _ string) error {
		refreshed = append(refreshed, auth.ID)
		if auth.ID == "missing" {
			_, errApply := manager.ApplyProviderQuotaReport(context.Background(), auth.ID, true, time.Now().Add(24*time.Hour))
			return errApply
		}
		return nil
	})
	candidates, preparation, errPrepare := manager.prepareQuotaRecoveryCandidates(context.Background(), manager.List(), "gpt-5.5")
	if errPrepare != nil {
		t.Fatalf("prepare candidates: %v", errPrepare)
	}
	if len(candidates) != 2 {
		t.Fatalf("prepared candidate count = %d, want 2", len(candidates))
	}
	if len(refreshed) != 1 || refreshed[0] != "missing" {
		t.Fatalf("refreshed IDs = %v, want [missing]", refreshed)
	}
	if preparation.refreshTried != 1 || preparation.refreshed != 1 || preparation.refreshError != 0 {
		t.Fatalf("refresh preparation = %#v, want one successful refresh", preparation)
	}
}

func TestPrepareQuotaRecoveryCandidatesKeepsFailedRefreshForFallback(t *testing.T) {
	manager := NewManager(nil, quotaRecoveryTestSelector{}, nil)
	for _, auth := range []*Auth{
		{ID: "cached", Provider: "codex", Quota: QuotaState{NextRecoverAt: time.Now().Add(time.Hour), ManualReset: ManualResetObservation{Provider: "codex", ObservedAt: time.Now()}}},
		{ID: "failed", Provider: "codex"},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register: %v", errRegister)
		}
	}
	manager.SetQuotaRecoveryRefresher(func(context.Context, *Auth, string) error { return errors.New("provider unavailable") })
	candidates, preparation, errPrepare := manager.prepareQuotaRecoveryCandidates(context.Background(), manager.List(), "gpt-5.5")
	if errPrepare != nil {
		t.Fatalf("prepare error = %v", errPrepare)
	}
	if len(candidates) != 2 || preparation.refreshTried != 1 || preparation.refreshError != 1 {
		t.Fatalf("failed refresh preparation = candidates=%d preparation=%#v", len(candidates), preparation)
	}
}

func TestGSRRefreshesManualResetWithKnownWeeklyReset(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			now := time.Now().UTC()
			manager := NewManager(nil, &GreedySoonestRecoverySelector{}, nil)
			for _, id := range []string{"first", "second"} {
				_, errRegister := manager.Register(context.Background(), &Auth{
					ID: id, Provider: provider, Quota: QuotaState{NextRecoverAt: now.Add(6 * 24 * time.Hour)},
				})
				if errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			manager.SetQuotaRecoveryRefresher(func(ctx context.Context, auth *Auth, _ string) error {
				quota := auth.Quota.Clone()
				quota.ManualReset = ManualResetObservation{Provider: provider, ObservedAt: now, ExpiresAt: now.Add(48 * time.Hour)}
				_, errUpdate := manager.UpdateQuotaObservation(ctx, auth.ID, quota)
				return errUpdate
			})
			_, preparation, errPrepare := manager.prepareQuotaRecoveryCandidates(context.Background(), manager.List(), "test-model")
			if errPrepare != nil || preparation.refreshed != 2 {
				t.Fatalf("initial refresh = %+v, %v; want two manual reset checks", preparation, errPrepare)
			}
			_, preparation, errPrepare = manager.prepareQuotaRecoveryCandidates(context.Background(), manager.List(), "test-model")
			if errPrepare != nil || preparation.refreshTried != 0 {
				t.Fatalf("cached refresh = %+v, %v; want no checks", preparation, errPrepare)
			}

			// Age the observation explicitly instead of waiting for cache expiry.
			for _, auth := range manager.List() {
				auth.Quota.ManualReset.ObservedAt = now.Add(-4 * time.Minute)
				if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
					t.Fatal(errUpdate)
				}
			}
			_, preparation, errPrepare = manager.prepareQuotaRecoveryCandidates(context.Background(), manager.List(), "test-model")
			if errPrepare != nil || preparation.refreshed != 2 {
				t.Fatalf("stale refresh = %+v, %v; want two checks", preparation, errPrepare)
			}
		})
	}
}

func TestGSRManualResetSurvivesTrafficAndClearsWhenSpent(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	manager := NewManager(nil, nil, nil)
	manual := ManualResetObservation{Provider: "codex", ObservedAt: now, ExpiresAt: now.Add(48 * time.Hour)}
	auth, errRegister := manager.Register(context.Background(), &Auth{ID: "manual", Provider: "codex", Quota: QuotaState{ManualReset: manual}})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	quota := QuotaState{}
	quota.ObserveResponseHeadersForProvider("codex", http.Header{
		"X-Codex-Secondary-Reset-At": []string{now.Add(6 * 24 * time.Hour).Format(time.RFC3339)},
	}, now.Add(time.Second))
	updated, errUpdate := manager.UpdateQuotaObservation(context.Background(), auth.ID, quota)
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if updated.Quota.ManualReset != manual {
		t.Fatalf("response headers lost manual reset observation: %+v", updated.Quota)
	}
	if got, ok := authRecoveryTimestampAt(updated, "codex", "gpt-5", now.Add(time.Second)); !ok || !got.Equal(manual.ExpiresAt) {
		t.Fatalf("post-traffic recovery = %v, %v; want manual expiry", got, ok)
	}
	quota = updated.Quota.Clone()
	quota.ManualReset = ManualResetObservation{Provider: "codex", ObservedAt: now.Add(2 * time.Second)}
	updated, errUpdate = manager.UpdateQuotaObservation(context.Background(), auth.ID, quota)
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if got, ok := authRecoveryTimestampAt(updated, "codex", "gpt-5", now.Add(2*time.Second)); !ok || !got.Equal(now.Add(6*24*time.Hour)) {
		t.Fatalf("spent credit recovery = %v, %v; want weekly reset", got, ok)
	}
	// A delayed observation must not restore a spent credit.
	quota.ManualReset = manual
	updated, errUpdate = manager.UpdateQuotaObservation(context.Background(), auth.ID, quota)
	if errUpdate != nil || !updated.Quota.ManualReset.ExpiresAt.IsZero() {
		t.Fatalf("stale credit restored: %+v, %v", updated, errUpdate)
	}
}

func strconvFormatInt(value int64) string {
	return fmt.Sprintf("%d", value)
}
