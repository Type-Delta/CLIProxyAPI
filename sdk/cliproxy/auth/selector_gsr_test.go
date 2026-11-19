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

func TestGreedySoonestRecoverySelectorScalesObservedQuotaByPlan(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)
	auths := []*Auth{
		{ID: "plus", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}, Quota: QuotaState{Signals: map[string]string{
			"X-Codex-Primary-Reset-At": now.Format(time.RFC3339), "X-Codex-Primary-Used-Percent": "0",
		}}},
		{ID: "pro", Provider: "codex", Attributes: map[string]string{"plan_type": "pro"}, Quota: QuotaState{Signals: map[string]string{
			"X-Codex-Primary-Reset-At": now.Format(time.RFC3339), "X-Codex-Primary-Used-Percent": "90",
		}}},
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

func TestGSRPrefersMoreRemainingQuota(t *testing.T) {
	reset := time.Now().Add(time.Hour).Format(time.RFC3339)
	pool := []*Auth{
		{ID: "plus", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}, Quota: QuotaState{Signals: map[string]string{
			"X-Codex-Secondary-Reset-At": reset, "X-Codex-Secondary-Used-Percent": "20",
		}}},
		{ID: "pro", Provider: "codex", Attributes: map[string]string{"plan_type": "pro"}, Quota: QuotaState{Signals: map[string]string{
			"X-Codex-Secondary-Reset-At": reset, "X-Codex-Secondary-Used-Percent": "99",
		}}},
	}
	selector := &GreedySoonestRecoverySelector{}
	picked, errPick := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, pool)
	if errPick != nil {
		t.Fatal(errPick)
	}
	if picked.ID != "plus" {
		t.Fatalf("picked %q, want Plus with 0.8 Plus-equivalent quota over Pro with 0.2", picked.ID)
	}
	// A nearly depleted short window caps the amount this session can use.
	pool[0].Quota.Signals["X-Codex-Primary-Reset-At"] = reset
	pool[0].Quota.Signals["X-Codex-Primary-Used-Percent"] = "95"
	factors, errFactors := greedySoonestRecoveryFactors(pool, "codex", "gpt-5", time.Now(), 0)
	if errFactors != nil || factors["plus"].Weight != 50_001 || factors["pro"].Weight != 200_001 {
		t.Fatalf("limiting quota weights = %+v, err = %v", factors, errFactors)
	}
	// Expired and malformed observations must not restore a plan-only bonus.
	pool[1].Quota.Signals["X-Codex-Secondary-Used-Percent"] = "NaN"
	pool[1].Quota.Signals["X-Codex-Primary-Reset-At"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
	pool[1].Quota.Signals["X-Codex-Primary-Used-Percent"] = "0"
	factors, errFactors = greedySoonestRecoveryFactors(pool, "codex", "gpt-5", time.Now(), 0)
	if errFactors != nil || factors["pro"].Weight != 1_000_001 || factors["pro"].RemainingQuotaFraction != nil {
		t.Fatalf("unobserved quota factors = %+v, err = %v", factors["pro"], errFactors)
	}
}

func TestGSRUsesSparkWebSocketQuota(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour).Format(time.RFC3339)
	auth := &Auth{ID: "spark", Provider: "codex", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"X-Codex-Primary-Used-Percent":                                       "99",
		"X-Codex-Primary-Reset-At":                                           reset,
		"X-Codex-Additional-Gpt-5-3-Codex-Spark-Primary-Used-Percent":        "40",
		"X-Codex-Additional-Gpt-5-3-Codex-Spark-Primary-Reset-After-Seconds": "900",
	}}}
	remaining, observed := gsrRemainingFraction(auth, "codex", "gpt-5.3-codex-spark", now)
	if !observed || remaining != 0.6 {
		t.Fatalf("Spark remaining quota = %g, observed %t, want 0.6", remaining, observed)
	}
}

func TestGSRFableUsesSharedAndScopedQuota(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour)
	auth := &Auth{Provider: "claude", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Reset":          reset.Format(time.RFC3339),
		"Anthropic-Ratelimit-Unified-5h-Utilization":    "0.99",
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":       reset.Format(time.RFC3339),
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": "0.2",
	}}}
	check := func() {
		t.Helper()
		remaining, observed := gsrRemainingFraction(auth, "claude", "claude-fable-5-1", now)
		if !observed || remaining < 0.009999 || remaining > 0.010001 {
			t.Fatalf("Fable remaining quota = %g, observed %t, want 0.01 from shared window", remaining, observed)
		}
	}
	check()
	auth.Quota.SetGSRQuotaWindows("claude", []QuotaReserveBar{
		{ID: "five-hour", RemainingPercent: 1, ResetAt: reset},
		{ID: "seven-day-fable", RemainingPercent: 80, ResetAt: reset},
	})
	check()
}

func TestGSRAddsSchedulingWeightAfterRecoveryAndTier(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		attributes map[string]string
		metadata   map[string]any
		delay      time.Duration
		want       int64
	}{
		{name: "default", want: 1_000_001},
		{name: "attribute", attributes: map[string]string{AttributeWeight: "5"}, want: 1_000_005},
		{name: "metadata", metadata: map[string]any{AttributeWeight: float64(7)}, want: 1_000_007},
		{name: "attribute precedence", attributes: map[string]string{AttributeWeight: "3"}, metadata: map[string]any{AttributeWeight: float64(7)}, want: 1_000_003},
		{name: "zero", attributes: map[string]string{AttributeWeight: "0"}, want: 1_000_000},
		{name: "negative", attributes: map[string]string{AttributeWeight: "-5"}, want: 1_000_000},
		{name: "invalid", attributes: map[string]string{AttributeWeight: "invalid"}, want: 1_000_000},
		{name: "over maximum", attributes: map[string]string{AttributeWeight: "1000001"}, want: 1_000_000},
		{name: "maximum and elite tier", attributes: map[string]string{AttributeWeight: "1000000", "gsr_tier": "elite"}, want: 13_000_000},
		{name: "later recovery and standard tier", attributes: map[string]string{AttributeWeight: "5", "gsr_tier": "standard"}, delay: 72 * time.Hour, want: 270_675},
	} {
		t.Run(test.name, func(t *testing.T) {
			auths := []*Auth{
				{ID: "anchor", Provider: "zai", Quota: QuotaState{Signals: map[string]string{"weekly_reset_at": now.Add(24 * time.Hour).Format(time.RFC3339)}}},
				{ID: "candidate", Provider: "zai", Attributes: test.attributes, Metadata: test.metadata, Quota: QuotaState{Signals: map[string]string{"weekly_reset_at": now.Add(24*time.Hour + test.delay).Format(time.RFC3339)}}},
			}
			_, weights, err := greedySoonestRecoveryWeights(auths, "zai", "glm-5", now, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := weights["candidate"]; got != test.want {
				t.Errorf("selector weight = %d, want %d", got, test.want)
			}
			entries := make([]*scheduledAuth, 0, len(auths))
			for _, auth := range auths {
				entries = append(entries, &scheduledAuth{auth: auth, meta: buildScheduledAuthMeta(auth)})
			}
			_, scheduledWeights, errScheduled := greedySoonestRecoveryScheduledWeights(entries, nil, "glm-5", now)
			if errScheduled != nil {
				t.Fatal(errScheduled)
			}
			if got := scheduledWeights["candidate"]; got != test.want {
				t.Errorf("scheduler weight = %d, want %d", got, test.want)
			}
		})
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

func TestGSRManualResetExpiry(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	weekly := now.Add(6 * 24 * time.Hour)
	manual := now.Add(2 * 24 * time.Hour)
	for _, test := range []struct {
		name, provider, model, signal, manualProvider string
		expires, observed, want                       time.Time
	}{
		{"codex earlier credit", "codex", "gpt-5", "X-Codex-Secondary-Reset-At", "codex", manual, now, manual},
		{"claude earlier grant", "claude", "claude-sonnet-4-5", "Anthropic-Ratelimit-Unified-7d-Reset", "claude", manual, now, manual},
		{"later credit", "codex", "gpt-5", "X-Codex-Secondary-Reset-At", "codex", weekly.Add(time.Hour), now, weekly},
		{"expired credit", "codex", "gpt-5", "X-Codex-Secondary-Reset-At", "codex", now, now, weekly},
		{"missing expiry", "codex", "gpt-5", "X-Codex-Secondary-Reset-At", "codex", time.Time{}, now, weekly},
		{"unobserved credit", "codex", "gpt-5", "X-Codex-Secondary-Reset-At", "codex", manual, time.Time{}, weekly},
		{"future observation", "codex", "gpt-5", "X-Codex-Secondary-Reset-At", "codex", manual, now.Add(time.Hour), weekly},
		{"wrong quota provider", "codex", "gpt-5", "X-Codex-Secondary-Reset-At", "claude", manual, now, weekly},
		{"fable independent allowance", "claude", "claude-fable-5-1", "Anthropic-Ratelimit-Unified-7d_oi-Reset", "claude", manual, now, weekly},
		{"spark independent allowance", "codex", "gpt-5.3-codex-spark", "X-Codex-Secondary-Reset-At", "codex", manual, now, weekly},
		{"code review independent allowance", "codex", "codex-auto-review", "X-Codex-Secondary-Reset-At", "codex", manual, now, weekly},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := &Auth{Provider: test.provider, Quota: QuotaState{
				Signals: map[string]string{test.signal: weekly.Format(time.RFC3339)},
				ManualReset: ManualResetObservation{Provider: test.manualProvider,
					ObservedAt: test.observed, ExpiresAt: test.expires},
			}}
			got, ok := authRecoveryTimestampAt(auth, "mixed", test.model, now)
			if !ok || !got.Equal(test.want) {
				t.Fatalf("recovery = %v, %v; want %v", got, ok, test.want)
			}
			if auth.Quota.Signals[test.signal] != weekly.Format(time.RFC3339) {
				t.Fatal("routing changed the reported weekly reset")
			}
		})
	}
}

func TestGSRManualResetGetsSameShareAsWeeklyReset(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	manual := now.Add(48 * time.Hour)
	auths := []*Auth{
		{ID: "manual", Provider: "codex", Quota: QuotaState{
			Signals:     map[string]string{"X-Codex-Secondary-Reset-At": now.Add(6 * 24 * time.Hour).Format(time.RFC3339)},
			ManualReset: ManualResetObservation{Provider: "codex", ObservedAt: now, ExpiresAt: manual},
		}},
		{ID: "weekly", Provider: "codex", Quota: QuotaState{
			Signals: map[string]string{"X-Codex-Secondary-Reset-At": manual.Format(time.RFC3339)},
		}},
	}
	selector := &GreedySoonestRecoverySelector{}
	counts := make(map[string]int)
	for i := 0; i < 100; i++ {
		picked, errPick := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
		if errPick != nil {
			t.Fatal(errPick)
		}
		counts[picked.ID]++
	}
	if counts["manual"] != 50 || counts["weekly"] != 50 {
		t.Fatalf("routing shares = %v, want 50:50", counts)
	}
}
