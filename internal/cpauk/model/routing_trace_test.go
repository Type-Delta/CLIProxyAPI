package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
)

func TestSanitizeRoutingTraceKeepsEmptyCandidatesAsArray(t *testing.T) {
	trace := &routing.Trace{Version: routing.TraceVersion, Mode: routing.ModeLocal, DecidedBy: routing.DecidedByNone, Candidates: []routing.Candidate{}, Pick: -1}
	encoded, truncated := SanitizeRoutingTrace(trace, [32]byte{}, true, "codex")
	if truncated {
		t.Fatal("small trace reported as truncated")
	}
	if !strings.Contains(string(encoded), `"candidates":[]`) {
		t.Fatalf("persisted trace = %s, want candidates as an empty array", encoded)
	}
}

func TestSanitizeRoutingTraceDropsStrategyDetailFirst(t *testing.T) {
	trace := &routing.Trace{Version: 1, Mode: routing.ModeDryRun, Pick: 0, Candidates: []routing.Candidate{{ID: "credential", Provider: "zai", Priority: 4, Gate: routing.GateCooldown, UntilMs: 123}}, Affinity: &routing.AffinityStep{State: routing.AffinityKept, Bound: 0, BoundRequestID: "0123456789abcdef0123456789abcdef", BoundAtMs: 123}, StrategyStep: &routing.StrategyStep{Applied: routing.StrategyGreedySoonestRecovery, Last: -1, Wanted: 0, Next: []int{0}, Weights: []routing.StrategyWeight{{Index: 0, Weight: 42, Share: 1, Credit: 17, RecoverySource: strings.Repeat("x", MaxRoutingTraceBytes), Tier: "pro", BaseWeight: 21, ConfigWeight: 1}}}}
	encoded, truncated := SanitizeRoutingTrace(trace, [32]byte{}, true, "zai")
	if truncated {
		t.Fatal("should fit after stripping factor details")
	}
	var got routing.Trace
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got.StrategyStep.Next != nil || got.StrategyStep.Weights[0].RecoverySource != "" || got.Candidates[0].Gate != routing.GateCooldown || got.Candidates[0].Priority != 4 {
		t.Fatalf("wrong truncation order %s", encoded)
	}
	if got.StrategyStep.Wanted != 0 || got.StrategyStep.Weights[0].Index != 0 || got.StrategyStep.Weights[0].Weight != 42 || got.Affinity.BoundRequestID != trace.Affinity.BoundRequestID {
		t.Fatalf("lost references %s", encoded)
	}
	if trace.StrategyStep.Next == nil || trace.StrategyStep.Weights[0].RecoverySource == "" {
		t.Fatal("sanitizer mutated input")
	}
}

func TestEventsProxyRequestIDFilterValidation(t *testing.T) {
	valid := `{"schema_version":1,"operation":"events","start":"2026-08-01T00:00:00Z","end":"2026-08-02T00:00:00Z","time_zone":"UTC","filters":{"proxy_request_id":["0123456789abcdef0123456789abcdef"]}}`
	if _, err := ParseQuery([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"invalid", "0123456789ABCDEF0123456789ABCDEF", ""} {
		if _, err := ParseQuery([]byte(strings.ReplaceAll(valid, "0123456789abcdef0123456789abcdef", bad))); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestSanitizeRoutingTraceStripsAnalyticsID(t *testing.T) {
	trace := &routing.Trace{Version: 1, Candidates: []routing.Candidate{{ID: "credential", Provider: "zai", AnalyticsID: "response-only"}}}
	for _, store := range []bool{false, true} {
		encoded, dropped := SanitizeRoutingTrace(trace, [32]byte{}, store, "zai")
		if dropped || strings.Contains(string(encoded), "analytics_id") || strings.Contains(string(encoded), "response-only") {
			t.Fatalf("persisted response-only field: %s", encoded)
		}
	}
	if trace.Candidates[0].AnalyticsID != "response-only" {
		t.Fatal("sanitizer mutated input")
	}
}
