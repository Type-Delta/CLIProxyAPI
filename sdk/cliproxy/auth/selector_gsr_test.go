package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestGreedySoonestRecoverySelectorUsesExponentialRecoveryShares(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	auths := []*Auth{
		{ID: "one-day", Provider: "zai", Quota: QuotaState{Signals: map[string]string{"weekly_reset_at": now.Add(24 * time.Hour).Format(time.RFC3339)}}},
		{ID: "two-days", Provider: "zai", Quota: QuotaState{Signals: map[string]string{"weekly_reset_at": now.Add(48 * time.Hour).Format(time.RFC3339)}}},
		{ID: "four-days", Provider: "zai", Quota: QuotaState{Signals: map[string]string{"weekly_reset_at": now.Add(96 * time.Hour).Format(time.RFC3339)}}},
	}
	selector := &GreedySoonestRecoverySelector{HalfLife: 36 * time.Hour}
	counts := make(map[string]int)
	for i := 0; i < 10000; i++ {
		picked, errPick := selector.Pick(context.Background(), "zai", "glm-5", cliproxyexecutor.Options{}, auths)
		if errPick != nil {
			t.Fatalf("Pick() iteration %d: %v", i, errPick)
		}
		counts[picked.ID]++
	}
	if !(counts["one-day"] > counts["two-days"] && counts["two-days"] > counts["four-days"]) {
		t.Fatalf("recovery share counts = %#v, want strictly decreasing with later recovery", counts)
	}
	if counts["four-days"] == 0 {
		t.Fatalf("latest recovery credential was starved: %#v", counts)
	}
}

func TestGreedySoonestRecoverySelectorFallsBackWhenRecoveryTimestampMissing(t *testing.T) {
	selector := &GreedySoonestRecoverySelector{}
	picked, errPick := selector.Pick(context.Background(), "zai", "glm-5", cliproxyexecutor.Options{}, []*Auth{{ID: "missing", Provider: "zai"}})
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if picked == nil || picked.ID != "missing" {
		t.Fatalf("Pick() = %#v, want missing auth", picked)
	}
}

func TestGreedySoonestRecoverySelectorPrefersAllowanceTier(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)
	auths := []*Auth{
		{ID: "plus", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}, Quota: QuotaState{Signals: map[string]string{"X-Codex-Primary-Reset-At": now.Format(time.RFC3339)}}},
		{ID: "pro", Provider: "codex", Attributes: map[string]string{"plan_type": "pro"}, Quota: QuotaState{Signals: map[string]string{"X-Codex-Primary-Reset-At": now.Format(time.RFC3339)}}},
	}
	selector := &GreedySoonestRecoverySelector{}
	counts := map[string]int{}
	for i := 0; i < 10000; i++ {
		picked, errPick := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
		if errPick != nil {
			t.Fatal(errPick)
		}
		counts[picked.ID]++
	}
	if counts["pro"] <= counts["plus"] {
		t.Fatalf("tier-aware counts = %#v, want pro > plus", counts)
	}
}

func TestAuthRecoveryTimestampUsesProviderWindowPreference(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	fable := &Auth{ID: "fable", Provider: "claude", Quota: QuotaState{Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset":    now.Add(4 * time.Hour).Format(time.RFC3339),
		"Anthropic-Ratelimit-Unified-7d_oi-Reset": now.Add(2 * time.Hour).Format(time.RFC3339),
	}}}
	if got, _ := AuthRecoveryTimestamp(fable, "claude", "claude-fable-5-1"); got.Sub(now) != 2*time.Hour {
		t.Fatalf("Fable recovery = %v, want 2h", got.Sub(now))
	}
	normal := fable.Clone()
	normal.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = now.Add(3 * time.Hour).Format(time.RFC3339)
	if got, _ := AuthRecoveryTimestamp(normal, "claude", "claude-sonnet-4-5"); got.Sub(now) != 3*time.Hour {
		t.Fatalf("normal Claude recovery = %v, want 3h", got.Sub(now))
	}
}
