package management

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
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
