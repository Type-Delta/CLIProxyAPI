package management

import "testing"

func TestNormalizeRoutingStrategyWeightedRoundRobin(t *testing.T) {
	for _, input := range []string{"weighted-round-robin", "weightedroundrobin", "wrr"} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "weighted-round-robin" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want weighted-round-robin, true", input, got, ok)
		}
	}
}

func TestNormalizeRoutingStrategyGreedySoonestRecovery(t *testing.T) {
	for _, input := range []string{"greedy-soonest-recovery", "greedysoonestrecovery", "gsr"} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "greedy-soonest-recovery" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want greedy-soonest-recovery, true", input, got, ok)
		}
	}
}
