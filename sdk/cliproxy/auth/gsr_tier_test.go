package auth

import "testing"

func TestGSRAllowanceTierForAuth(t *testing.T) {
	tests := []struct {
		provider string
		value    string
		want     GSRAllowanceTier
	}{
		{provider: "codex", value: "free", want: GSRAllowanceUnknown},
		{provider: "codex", value: "plus", want: GSRAllowanceStandard},
		{provider: "codex", value: "prolite", want: GSRAllowancePremium},
		{provider: "codex", value: "pro", want: GSRAllowanceElite},
		{provider: "claude", value: "pro", want: GSRAllowancePro},
		{provider: "claude", value: "max", want: GSRAllowanceMax},
		{provider: "zai", value: "lite", want: GSRAllowanceStandard},
		{provider: "opencode-go", value: "", want: GSRAllowanceUnknown},
		{provider: "kiro", value: "KIRO FREE", want: GSRAllowanceUnknown},
		{provider: "kiro", value: "KIRO PRO", want: GSRAllowancePro},
		{provider: "kiro", value: "KIRO PRO+", want: GSRAllowancePremium},
		{provider: "kiro", value: "KIRO PRO PLUS", want: GSRAllowancePremium},
		{provider: "kiro", value: "KIRO PROPLUS", want: GSRAllowancePremium},
		{provider: "kiro", value: "KIRO POWER", want: GSRAllowanceMax},
	}
	for _, test := range tests {
		auth := &Auth{Provider: test.provider, Attributes: map[string]string{"plan_type": test.value}}
		if got := GSRAllowanceTierForAuth(auth, test.provider); got != test.want {
			t.Fatalf("%s/%s tier = %d, want %d", test.provider, test.value, got, test.want)
		}
	}
}

func TestGSRAllowanceTierForAuthKiroPlanSignal(t *testing.T) {
	auth := &Auth{Provider: "kiro", Quota: QuotaState{Signals: map[string]string{"kiro_plan_type": "KIRO POWER"}}}
	if got := GSRAllowanceTierForAuth(auth, "kiro"); got != GSRAllowanceMax {
		t.Fatalf("Kiro plan signal tier = %d, want max", got)
	}
}

func TestGSRAllowanceOverrideWins(t *testing.T) {
	auth := &Auth{Provider: "codex", Attributes: map[string]string{"plan_type": "free", "gsr_tier": "max"}}
	if got := GSRAllowanceTierForAuth(auth, "codex"); got != GSRAllowanceMax {
		t.Fatalf("override tier = %d, want max", got)
	}
}

func TestGSRAllowanceUnknownIsNeutral(t *testing.T) {
	if got := gsrAllowanceMultiplier(GSRAllowanceUnknown); got != 1 {
		t.Fatalf("unknown multiplier = %d, want neutral 1", got)
	}
}
