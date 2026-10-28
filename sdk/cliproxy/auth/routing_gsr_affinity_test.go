package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestRoutingGSRAffinityBeforeRulesAndRefresh(t *testing.T) {
	for _, test := range []struct {
		name          string
		mixed         bool
		lowerPriority string
	}{
		{"single/same-tier", false, "2"}, {"single/across-tiers", false, "1"},
		{"mixed/same-tier", true, "2"}, {"mixed/across-tiers", true, "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			const provider = "gsr-lcp-rule-test"
			const model = "gsr-lcp-rule-model"
			affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &GreedySoonestRecoverySelector{}, TTL: time.Hour})
			defer affinity.Stop()
			manager := NewManager(nil, affinity, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
			for _, auth := range []*Auth{
				{ID: "gsr-bound", Provider: provider, Status: StatusActive},
				{ID: "gsr-target-a", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "2"}},
				{ID: "gsr-target-b", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": test.lowerPriority}},
				{ID: "gsr-outside", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "10"}},
			} {
				if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
				id := auth.ID
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}
			cfg, err := internalconfig.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [gsr-target-a, gsr-target-b]\n"))
			if err != nil {
				t.Fatal(err)
			}
			manager.SetConfig(cfg)
			makeOpts := func(text string) cliproxyexecutor.Options {
				return cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: []byte(text), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "gsr-lcp-caller"}}
			}
			seed := makeOpts(`{"messages":[{"role":"user","content":"start"}]}`)
			_, err = affinity.Pick(context.Background(), provider, model, seed, []*Auth{{ID: "gsr-bound", Provider: provider, Status: StatusActive}})
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var refreshed []string
			manager.SetQuotaRecoveryRefresher(func(ctx context.Context, a *Auth, _ string) error {
				mu.Lock()
				refreshed = append(refreshed, a.ID)
				mu.Unlock()
				if a.ID == "gsr-target-a" {
					a.Disabled = true
					_, errUpdate := manager.Update(WithSkipPersist(ctx), a)
					return errUpdate
				}
				return nil
			})
			pick := func(ctx context.Context, opts cliproxyexecutor.Options) (*Auth, error) {
				if test.mixed {
					a, _, _, errPick := manager.pickNextMixed(ctx, []string{provider}, model, opts, nil)
					return a, errPick
				}
				a, _, errPick := manager.pickNext(ctx, provider, model, opts, nil)
				return a, errPick
			}
			ctx, holder := routing.WithTraceHolder(context.Background())
			bound, err := pick(ctx, makeOpts(`{"messages":[{"role":"user","content":"start"},{"role":"assistant","content":"answer"},{"role":"user","content":"continue"}]}`))
			if err != nil || bound == nil || bound.ID != "gsr-bound" {
				t.Fatalf("bound=%v err=%v", bound, err)
			}
			trace := holder.Last()
			if trace == nil || trace.Affinity.State != routing.AffinityKept || trace.DecidedBy != routing.DecidedByAffinity || trace.Rules == nil || trace.Rules.Applied != -1 || len(trace.Rules.Matched) != 1 || trace.Rules.Matched[0].Status != routing.RuleOverridden {
				t.Fatalf("trace=%+v", trace)
			}
			mu.Lock()
			got := append([]string(nil), refreshed...)
			mu.Unlock()
			if len(got) != 0 {
				t.Fatalf("refreshed despite eligible binding: %v", got)
			}
			next, err := pick(context.Background(), makeOpts(`{"messages":[{"role":"user","content":"unrelated"}]}`))
			if err != nil || next == nil || next.ID != "gsr-target-b" {
				t.Fatalf("unbound selection=%v err=%v", next, err)
			}
			mu.Lock()
			got = append([]string(nil), refreshed...)
			mu.Unlock()
			if len(got) != 2 {
				t.Fatalf("refreshes=%v, want both rule targets", got)
			}
			for _, id := range got {
				if id != "gsr-target-a" && id != "gsr-target-b" {
					t.Fatalf("refreshed outside rule pool: %v", got)
				}
			}
		})
	}
}

func TestGSRAffinityWithoutRulesKeepsExistingRefreshBehavior(t *testing.T) {
	const provider = "gsr-baseline-test"
	const model = "gsr-baseline-model"
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &GreedySoonestRecoverySelector{}, TTL: time.Hour})
	defer affinity.Stop()
	manager := NewManager(nil, affinity, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
	for _, id := range []string{"baseline-a", "baseline-b"} {
		if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: provider, Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		id := id
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	makeOpts := func(text string) cliproxyexecutor.Options {
		return cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: []byte(text), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "baseline-caller"}}
	}
	_, err := affinity.Pick(context.Background(), provider, model, makeOpts(`{"messages":[{"role":"user","content":"hello"}]}`), []*Auth{{ID: "baseline-b", Provider: provider, Status: StatusActive}})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	refreshed := map[string]bool{}
	manager.SetQuotaRecoveryRefresher(func(_ context.Context, a *Auth, _ string) error {
		mu.Lock()
		refreshed[a.ID] = true
		mu.Unlock()
		return nil
	})
	opts := makeOpts(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"response"},{"role":"user","content":"continue"}]}`)
	selected, _, err := manager.pickNext(context.Background(), provider, model, opts, nil)
	if err != nil || selected == nil || selected.ID != "baseline-b" {
		t.Fatalf("selection=%v err=%v", selected, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !refreshed["baseline-a"] || !refreshed["baseline-b"] || len(refreshed) != 2 {
		t.Fatalf("baseline refreshes=%v", refreshed)
	}
}

func TestRoutingGSRExplicitBindingWinsBeforeRefresh(t *testing.T) {
	const provider = "gsr-explicit-test"
	const model = "gsr-explicit-model"
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &GreedySoonestRecoverySelector{}, TTL: time.Hour})
	defer affinity.Stop()
	manager := NewManager(nil, affinity, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
	for _, id := range []string{"explicit-bound", "explicit-a", "explicit-b"} {
		if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: provider, Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		id := id
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-ID": []string{"explicit-session"}}, Metadata: map[string]any{}}
	_, err := affinity.Pick(context.Background(), provider, model, opts, []*Auth{{ID: "explicit-bound", Provider: provider, Status: StatusActive}})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := internalconfig.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [explicit-a, explicit-b]\n"))
	if err != nil {
		t.Fatal(err)
	}
	manager.SetConfig(cfg)
	var mu sync.Mutex
	refreshes := 0
	manager.SetQuotaRecoveryRefresher(func(_ context.Context, _ *Auth, _ string) error { mu.Lock(); refreshes++; mu.Unlock(); return nil })
	ctx, holder := routing.WithTraceHolder(context.Background())
	selected, _, err := manager.pickNext(ctx, provider, model, opts, nil)
	if err != nil || selected == nil || selected.ID != "explicit-bound" {
		t.Fatalf("selection=%v err=%v", selected, err)
	}
	trace := holder.Last()
	if trace == nil || trace.Affinity.State != routing.AffinityKept || trace.DecidedBy != routing.DecidedByAffinity {
		t.Fatalf("trace=%+v", trace)
	}
	mu.Lock()
	defer mu.Unlock()
	if refreshes != 0 {
		t.Fatalf("refreshed %d times despite binding", refreshes)
	}
}

func TestRoutingGSRWithoutAffinityRevalidatesOnlyRulePool(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		name := "single"
		if mixed {
			name = "mixed"
		}
		t.Run(name, func(t *testing.T) {
			const provider = "gsr-unbound-test"
			const model = "gsr-unbound-model"
			manager := NewManager(nil, &GreedySoonestRecoverySelector{}, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
			for _, a := range []*Auth{
				{ID: "unbound-outside", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "10"}},
				{ID: "unbound-a", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "2"}},
				{ID: "unbound-b", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "1"}},
			} {
				if _, err := manager.Register(WithSkipPersist(context.Background()), a); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(a.ID, provider, []*registry.ModelInfo{{ID: model}})
				id := a.ID
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}
			cfg, err := internalconfig.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [unbound-a, unbound-b]\n"))
			if err != nil {
				t.Fatal(err)
			}
			manager.SetConfig(cfg)
			var mu sync.Mutex
			refreshed := map[string]bool{}
			manager.SetQuotaRecoveryRefresher(func(ctx context.Context, a *Auth, _ string) error {
				mu.Lock()
				refreshed[a.ID] = true
				mu.Unlock()
				if a.ID == "unbound-a" {
					a.Disabled = true
					_, errUpdate := manager.Update(WithSkipPersist(ctx), a)
					return errUpdate
				}
				return nil
			})
			var selected *Auth
			if mixed {
				selected, _, _, err = manager.pickNextMixed(context.Background(), []string{provider}, model, cliproxyexecutor.Options{}, nil)
			} else {
				selected, _, err = manager.pickNext(context.Background(), provider, model, cliproxyexecutor.Options{}, nil)
			}
			if err != nil || selected == nil || selected.ID != "unbound-b" {
				t.Fatalf("selection=%v err=%v", selected, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(refreshed) != 2 || !refreshed["unbound-a"] || !refreshed["unbound-b"] {
				t.Fatalf("refreshed=%v", refreshed)
			}
		})
	}
}

func TestRoutingGSRRefreshFallsThroughWhenTargetsBecomeUnavailable(t *testing.T) {
	for _, affinityEnabled := range []bool{false, true} {
		name := "without-affinity"
		if affinityEnabled {
			name = "with-affinity"
		}
		t.Run(name, func(t *testing.T) {
			const provider = "gsr-closed-test"
			const model = "gsr-closed-model"
			var selector Selector = &GreedySoonestRecoverySelector{}
			if affinityEnabled {
				affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: selector, TTL: time.Hour})
				defer affinity.Stop()
				selector = affinity
			}
			manager := NewManager(nil, selector, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
			for _, id := range []string{"closed-a", "closed-b", "closed-outside"} {
				if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: provider, Status: StatusActive}); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
				id := id
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}
			cfg, err := internalconfig.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [closed-a, closed-b]\n"))
			if err != nil {
				t.Fatal(err)
			}
			manager.SetConfig(cfg)
			manager.SetQuotaRecoveryRefresher(func(ctx context.Context, a *Auth, _ string) error {
				a.Disabled = true
				_, errUpdate := manager.Update(WithSkipPersist(ctx), a)
				return errUpdate
			})
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: []byte(`{"messages":[{"role":"user","content":"new request"}]}`), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "closed-caller"}}
			selected, _, err := manager.pickNext(context.Background(), provider, model, opts, nil)
			if err != nil || selected == nil || selected.ID != "closed-outside" {
				t.Fatalf("selected=%v err=%v; want eligible fallback when both targets are blocked", selected, err)
			}
		})
	}
}
