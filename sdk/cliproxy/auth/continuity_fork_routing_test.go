package auth

import (
	"context"
	"reflect"
	"testing"
	"time"

	cfg "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
)

func TestContinuityUsesForkRoutingPool(t *testing.T) {
	for _, strategy := range []string{"round-robin", "weighted-round-robin", "gsr", "affinity"} {
		t.Run(strategy, func(t *testing.T) {
			var selector Selector = &RoundRobinSelector{}
			switch strategy {
			case "weighted-round-robin":
				selector = &WeightedRoundRobinSelector{}
			case "gsr":
				selector = &GreedySoonestRecoverySelector{}
			case "affinity":
				a := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour})
				defer a.Stop()
				selector = a
			}
			m := NewManager(nil, selector, nil)
			m.RegisterExecutor(&continuityTestExecutor{provider: "codex"})
			model := "continuity-fork-" + strategy
			for _, a := range []*Auth{
				{ID: "outside", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "10"}},
				{ID: "target", Provider: "codex", Status: StatusActive, Attributes: map[string]string{AttributeQuotaReserve: `{"weekly":30}`, AttributeQuotaReserveBypassKeys: `["` + quotaReserveBypassID + `"]`}},
			} {
				if _, err := m.Register(WithSkipPersist(t.Context()), a); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
			}
			config := &cfg.Config{}
			config.Routing.Rules = []cfg.RoutingRule{{RouteTo: []string{"target"}}}
			m.SetConfig(config)
			opts := ex.Options{Metadata: map[string]any{ex.ClientKeyIDMetadataKey: quotaReserveBypassID}}
			candidates := m.buildCodexContinuityCandidates(t.Context(), []string{"codex"}, model, opts, 0, false, 0, time.Now())
			if len(candidates) != 1 || candidates[0].ID != "target" {
				t.Fatalf("rule candidates: %+v", candidates)
			}
			trace, err := m.DryRunRoute(t.Context(), routing.DryRunRequest{Model: model, KeyID: quotaReserveBypassID})
			if err != nil {
				t.Fatal(err)
			}
			if trace.Pick < 0 || trace.Candidates[trace.Pick].ID != "target" {
				t.Fatalf("dry run disagrees: %+v", trace)
			}
			selected, _, err := m.pickNext(t.Context(), "codex", model, opts, nil)
			if err != nil || selected.ID != "target" {
				t.Fatalf("live selection disagrees: %v %v", selected, err)
			}
			target, _ := m.GetByID("target")
			m.ApplyQuotaReserveBars(target, []QuotaReserveBar{{ID: "weekly", RemainingPercent: 20, ResetAt: time.Now().Add(time.Hour)}})
			candidates = m.buildCodexContinuityCandidates(t.Context(), []string{"codex"}, model, ex.Options{}, 0, false, 0, time.Now())
			if len(candidates) != 1 || candidates[0].ID != "outside" {
				t.Fatalf("reserve rule fallthrough: %+v", candidates)
			}
			candidates = m.buildCodexContinuityCandidates(t.Context(), []string{"codex"}, model, opts, 0, false, 0, time.Now())
			if len(candidates) != 1 || candidates[0].ID != "target" {
				t.Fatalf("reserve bypass: %+v", candidates)
			}
			pinned := ex.Options{Metadata: map[string]any{ex.PinnedAuthMetadataKey: "outside"}}
			candidates = m.buildCodexContinuityCandidates(t.Context(), []string{"codex"}, model, pinned, 0, false, 0, time.Now())
			if len(candidates) != 1 || candidates[0].ID != "outside" {
				t.Fatalf("pin bypass of rule: %+v", candidates)
			}
			excluded := withHomeExcludedAuthIDs(opts, map[string]struct{}{"target": {}})
			candidates = m.buildCodexContinuityCandidates(t.Context(), []string{"codex"}, model, excluded, 0, false, 0, time.Now())
			if len(candidates) != 1 || candidates[0].ID != "outside" {
				t.Fatalf("retry exclusion: %+v", candidates)
			}
			if got := m.buildCodexContinuityCandidates(t.Context(), []string{"codex"}, model, opts, 1, true, 0, time.Now()); len(got) != 0 {
				t.Fatalf("retry limit ignored: %+v", got)
			}
		})
	}
}

func TestContinuityMixedRulesAndAffinity(t *testing.T) {
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour})
	defer affinity.Stop()
	m := NewManager(nil, affinity, nil)
	const model = "continuity-mixed-affinity"
	for _, a := range []*Auth{{ID: "codex-bound", Provider: "codex", Status: StatusActive}, {ID: "claude-rule", Provider: "claude", Status: StatusActive}} {
		m.RegisterExecutor(&continuityTestExecutor{provider: a.Provider})
		if _, err := m.Register(WithSkipPersist(context.Background()), a); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	}
	config := &cfg.Config{}
	config.Routing.Rules = []cfg.RoutingRule{{RouteTo: []string{"claude-rule"}}}
	m.SetConfig(config)
	if got := m.buildCodexContinuityCandidates(t.Context(), []string{"codex", "claude"}, model, ex.Options{}, 0, false, 0, time.Now()); len(got) != 0 {
		t.Fatalf("mixed rule exposes Codex: %+v", got)
	}
	opts := ex.Options{Metadata: map[string]any{ex.DerivedSessionIDMetadataKey: "bound-session"}}
	affinity.cache.Set("mixed::derived:bound-session::"+model, "codex-bound")
	before := affinity.cache.entries["mixed::derived:bound-session::"+model]
	got := m.buildCodexContinuityCandidates(t.Context(), []string{"codex", "claude"}, model, opts, 0, false, 0, time.Now())
	if len(got) != 1 || got[0].ID != "codex-bound" {
		t.Fatalf("affinity must precede mixed rule: %+v", got)
	}
	after := affinity.cache.entries["mixed::derived:bound-session::"+model]
	if before.authID != after.authID || !reflect.DeepEqual(before.binding, after.binding) {
		t.Fatal("candidate inspection changed binding")
	}
}
