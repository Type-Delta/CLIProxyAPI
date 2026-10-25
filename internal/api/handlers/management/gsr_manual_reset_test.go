package management

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestParseCodexManualResetExpiry(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, credits string
		want          time.Time
	}{
		{"earliest available", `[{"reset_type":"codex_rate_limits","status":"available","expires_at":"2030-01-04T00:00:00Z"},{"reset_type":"codex_rate_limits","status":"available","expires_at":"2030-01-03T00:00:00Z"}]`, now.Add(48 * time.Hour)},
		{"camel case", `[{"resetType":"codex_rate_limits","status":"available","expiresAt":"2030-01-03T00:00:00Z"}]`, now.Add(48 * time.Hour)},
		{"unix seconds", `[{"reset_type":"codex_rate_limits","status":"available","expires_at":1893628800}]`, now.Add(48 * time.Hour)},
		{"unix milliseconds", `[{"reset_type":"codex_rate_limits","status":"available","expires_at":1893628800000}]`, now.Add(48 * time.Hour)},
		{"spent", `[{"reset_type":"codex_rate_limits","status":"redeemed","expires_at":"2030-01-03T00:00:00Z"}]`, time.Time{}},
		{"wrong reset type", `[{"reset_type":"other","status":"available","expires_at":"2030-01-03T00:00:00Z"}]`, time.Time{}},
		{"expired", `[{"reset_type":"codex_rate_limits","status":"available","expires_at":"2030-01-01T00:00:00Z"}]`, time.Time{}},
		{"malformed date", `[{"reset_type":"codex_rate_limits","status":"available","expires_at":"invalid"}]`, time.Time{}},
		{"missing date", `[{"reset_type":"codex_rate_limits","status":"available"}]`, time.Time{}},
		{"no credits", `[]`, time.Time{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, errParse := parseManualResetExpiry("codex", []byte(`{"credits":`+test.credits+`}`), now)
			if errParse != nil || !got.Equal(test.want) {
				t.Fatalf("expiry = %v, %v; want %v", got, errParse, test.want)
			}
		})
	}
}

func TestParseClaudeManualResetExpiry(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, status, grant string
		want                time.Time
	}{
		{"available", `"eligible":true,"at_limit":true`, `"resets_left":1,"usable_now":true,"clears":["seven_day"]`, now.Add(48 * time.Hour)},
		{"healthy account can use after exhaustion", `"eligible":true,"at_limit":false`, `"resets_left":1,"usable_now":false,"use_requires_limit":true,"clears":["seven_day"]`, now.Add(48 * time.Hour)},
		{"ineligible", `"eligible":false`, `"resets_left":1,"usable_now":true,"clears":["seven_day"]`, time.Time{}},
		{"spent", `"eligible":true`, `"resets_left":0,"usable_now":true,"clears":["seven_day"]`, time.Time{}},
		{"paused", `"eligible":true`, `"resets_left":1,"paused":true,"usable_now":true,"clears":["seven_day"]`, time.Time{}},
		{"not started", `"eligible":true`, `"resets_left":1,"starts_at":"2030-01-02T00:00:00Z","usable_now":true,"clears":["seven_day"]`, time.Time{}},
		{"short window only", `"eligible":true`, `"resets_left":1,"usable_now":true,"clears":["five_hour"]`, time.Time{}},
		{"unusable at limit", `"eligible":true,"at_limit":true`, `"resets_left":1,"usable_now":false,"clears":["seven_day"]`, time.Time{}},
		{"unusable without limit condition", `"eligible":true`, `"resets_left":1,"usable_now":false,"use_requires_limit":false,"clears":["seven_day"]`, time.Time{}},
		{"cooldown outlasts grant", `"eligible":true,"cooldown_until":"2030-01-04T00:00:00Z"`, `"resets_left":1,"usable_now":true,"clears":["seven_day"]`, time.Time{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"cedar_ember":{%s,"grants":[{%s,"ends_at":"2030-01-03T00:00:00Z"}]}}`, test.status, test.grant)
			got, errParse := parseManualResetExpiry("claude", []byte(body), now)
			if errParse != nil || !got.Equal(test.want) {
				t.Fatalf("expiry = %v, %v; want %v", got, errParse, test.want)
			}
		})
	}
}

// Exercise provider HTTP reads, the shared cache, persisted observations and
// actual selector shares with the user's six-day weekly/two-day credit example.
func TestGSRManualResetProbeAndRouting(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			weekly, manual := now.Add(6*24*time.Hour), now.Add(48*time.Hour)
			fixture := newGSRProbeFixture(t)
			body := fmt.Sprintf(`{"rate_limit":{"secondary_window":{"window_minutes":10080,"reset_at":%d}},"seven_day":{"resets_at":%q},"credits":[{"reset_type":"codex_rate_limits","status":"available","expires_at":%q}],"cedar_ember":{"eligible":true,"grants":[{"resets_left":1,"usable_now":true,"clears":["seven_day"],"ends_at":%q}]}}`, weekly.Unix(), weekly.Format(time.RFC3339), manual.Format(time.RFC3339), manual.Format(time.RFC3339))
			fixture.setResponse(http.StatusOK, body)
			handler, auth := gsrProbeHandler(t, provider, map[string]string{"api_key": "fixture-key"})
			modelName, signal := "gpt-5", "X-Codex-Primary-Reset-At"
			if provider == "claude" {
				modelName, signal = "claude-sonnet-4-5", "Anthropic-Ratelimit-Unified-7d-Reset"
			}
			manager := handler.authManager
			if provider == "codex" {
				auth.Metadata = map[string]any{"account_id": "fixture-account"}
				if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
					t.Fatal(errUpdate)
				}
			}
			manager.SetSelector(&coreauth.GreedySoonestRecoverySelector{})
			manager.SetQuotaRecoveryRefresher(handler.RefreshQuotaForRouting)
			manager.RegisterExecutor(&refreshRecordExecutor{provider: provider})
			// Even an account with a known weekly deadline must load its credits.
			_, errUpdate := manager.UpdateQuotaObservation(context.Background(), auth.ID, coreauth.QuotaState{
				ObservedAt: now, Signals: map[string]string{signal: weekly.Format(time.RFC3339)},
			})
			if errUpdate != nil {
				t.Fatal(errUpdate)
			}
			control, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "two-day-weekly", Provider: provider, Quota: coreauth.QuotaState{
				Signals:     map[string]string{signal: manual.Format(time.RFC3339)},
				ManualReset: coreauth.ManualResetObservation{Provider: provider, ObservedAt: now},
			}})
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			for _, id := range []string{auth.ID, control.ID} {
				registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: modelName}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}
			counts := make(map[string]int)
			for i := 0; i < 100; i++ {
				picked, errPick := manager.SelectAuth(context.Background(), provider, modelName, cliproxyexecutor.Options{})
				if errPick != nil {
					t.Fatal(errPick)
				}
				counts[picked.ID]++
			}
			if counts[auth.ID] != 50 || counts[control.ID] != 50 {
				t.Fatalf("routing shares = %v, want 50:50", counts)
			}
			updated, _ := manager.GetByID(auth.ID)
			if got := coreauth.RecoveryTimestampForAuth(updated, modelName); !got.Equal(manual) {
				t.Fatalf("effective recovery = %v, want manual expiry %v", got, manual)
			}
			if updated.Quota.Signals[signal] != weekly.Format(time.RFC3339) {
				t.Fatalf("reported weekly reset was changed: %v", updated.Quota.Signals)
			}
			if errRefresh := handler.RefreshQuotaForRouting(context.Background(), updated, modelName); errRefresh != nil {
				t.Fatal(errRefresh)
			}
			if requests := fixture.requests(); len(requests) != 2 {
				t.Fatalf("provider requests = %v, want usage and reset reads with cached replay", requests)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			for _, headers := range fixture.headers {
				if headers.Get("Authorization") != "Bearer fixture-key" {
					t.Fatal("probe did not inject the selected account's credential")
				}
				if provider == "codex" && headers.Get("Chatgpt-Account-Id") != "fixture-account" {
					t.Fatal("Codex probe did not select the credential's account")
				}
				if provider == "claude" && (headers.Get("Anthropic-Beta") != "oauth-2025-04-20" || headers.Get("User-Agent") == "") {
					t.Fatal("Claude probe omitted its OAuth client headers")
				}
			}
		})
	}
}
