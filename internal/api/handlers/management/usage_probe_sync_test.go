package management

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSyncUsageProbeCooldown(t *testing.T) {
	futureReset := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	tests := []struct {
		name          string
		probe         string
		body          string
		staleReset    time.Time
		fetchedReset  time.Time
		wantExhausted bool
	}{
		{
			name:          "healthy response clears stale cooldown",
			probe:         "zai",
			body:          `{"code":200,"success":true,"data":{"limits":[{"unit":6,"number":1,"remaining":10,"percentage":10,"nextResetTime":1789615750984}]}}`,
			staleReset:    time.Now().Add(91 * time.Hour),
			wantExhausted: false,
		},
		{
			name:          "exhausted response overwrites stale cooldown",
			probe:         "opencode-go",
			body:          fmt.Sprintf(`{"usage":{"weekly":{"percent":100,"remaining":0,"resetsAt":%q}}}`, futureReset.Format(time.RFC3339)),
			staleReset:    time.Now().Add(2 * time.Hour),
			fetchedReset:  futureReset,
			wantExhausted: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			auth := &coreauth.Auth{
				ID:       "sync-usage-auth",
				Provider: "openai-compatible-test",
				Attributes: map[string]string{
					"usage_probe": test.probe,
				},
				ModelStates: map[string]*coreauth.ModelState{
					"deepseek-v4.1-flash": {
						Status:         coreauth.StatusError,
						Unavailable:    true,
						NextRetryAfter: test.staleReset,
						Quota: coreauth.QuotaState{
							Exceeded:      true,
							Reason:        "quota",
							NextRecoverAt: test.staleReset,
						},
					},
				},
			}
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}

			handler := &Handler{authManager: manager}
			target, errParse := url.Parse(zaiUsageQuotaURL)
			if test.probe == "opencode-go" {
				target, errParse = url.Parse(opencodeGoUsageQuotaURL)
			}
			if errParse != nil {
				t.Fatal(errParse)
			}
			handler.syncUsageProbeCooldown(context.Background(), auth, target, []byte(test.body))

			updated, ok := manager.GetByID(auth.ID)
			if !ok || updated == nil {
				t.Fatal("updated auth not found")
			}
			state := updated.ModelStates["deepseek-v4.1-flash"]
			if state == nil {
				t.Fatal("updated model state missing")
			}
			if test.wantExhausted {
				if !state.NextRetryAfter.Equal(test.fetchedReset) || !state.Quota.NextRecoverAt.Equal(test.fetchedReset) {
					t.Fatalf("cooldown = retry %v recover %v, want %v", state.NextRetryAfter, state.Quota.NextRecoverAt, test.fetchedReset)
				}
				return
			}
			if state.Unavailable || !state.NextRetryAfter.IsZero() || state.Quota.Exceeded {
				t.Fatalf("cooldown was not cleared: unavailable %v retry %v quota %+v", state.Unavailable, state.NextRetryAfter, state.Quota)
			}
		})
	}
}

func TestSyncClaudeUsageCooldownScopesFableAllowance(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	authID := "sync-claude-usage-auth"
	reset := time.Now().Add(2 * time.Hour).UTC()
	auth := &coreauth.Auth{
		ID: authID, Provider: "claude", Status: coreauth.StatusError,
		Unavailable: true, NextRetryAfter: reset,
		Quota: coreauth.QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: reset},
		ModelStates: map[string]*coreauth.ModelState{
			"claude-fable-5-1":  {Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: reset, Quota: coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: reset}},
			"claude-sonnet-4-5": {Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: reset, Quota: coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: reset}},
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, auth.Provider, []*registry.ModelInfo{{ID: "claude-fable-5-1"}, {ID: "claude-sonnet-4-5"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	handler := &Handler{authManager: manager}
	target, errParse := url.Parse(claudeUsageQuotaURL)
	if errParse != nil {
		t.Fatal(errParse)
	}
	body := fmt.Sprintf(`{"five_hour":{"utilization":25,"resets_at":%q},"seven_day":{"utilization":35,"resets_at":%q},"limits":[{"kind":"weekly_scoped","percent":100,"resets_at":%q,"is_active":true,"scope":{"model":{"display_name":"Fable"}}}]}`,
		reset.Format(time.RFC3339), reset.Format(time.RFC3339), reset.Format(time.RFC3339))
	handler.syncClaudeUsageCooldown(context.Background(), auth, target, []byte(body))

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.Quota.Reason == "credential_quota" || updated.Unavailable || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("credential cooldown = quota %+v unavailable %v retry %v, want credential scope clear", updated.Quota, updated.Unavailable, updated.NextRetryAfter)
	}
	if fable := updated.ModelStates["claude-fable-5-1"]; fable == nil || !fable.Quota.Exceeded {
		t.Fatalf("Fable state = %#v, want scoped cooldown", fable)
	}
	if sonnet := updated.ModelStates["claude-sonnet-4-5"]; sonnet == nil || sonnet.Quota.Exceeded || sonnet.Unavailable || !strings.EqualFold(string(sonnet.Status), string(coreauth.StatusActive)) {
		t.Fatalf("Sonnet state = %#v, want available", sonnet)
	}
}

func TestSyncClaudeUsageCooldownIgnoresModelSpecificFields(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	authID := "sync-claude-model-fields-auth"
	reset := time.Now().Add(2 * time.Hour).UTC()
	auth := &coreauth.Auth{
		ID: authID, Provider: "claude",
		ModelStates: map[string]*coreauth.ModelState{
			"claude-opus-5": {Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: reset, Quota: coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: reset}},
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, auth.Provider, []*registry.ModelInfo{{ID: "claude-opus-5"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	target, errParse := url.Parse(claudeUsageQuotaURL)
	if errParse != nil {
		t.Fatal(errParse)
	}
	body := fmt.Sprintf(`{"seven_day_opus":{"utilization":100,"resets_at":%q}}`, reset.Format(time.RFC3339))
	(&Handler{authManager: manager}).syncClaudeUsageCooldown(context.Background(), auth, target, []byte(body))

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	state := updated.ModelStates["claude-opus-5"]
	if state == nil || !state.Quota.Exceeded || !state.NextRetryAfter.Equal(reset) {
		t.Fatalf("Opus state = %#v, want unchanged model cooldown", state)
	}
}
