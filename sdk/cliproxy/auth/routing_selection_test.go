package auth

import (
	"context"
	"reflect"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestNarrowRoutingPool(t *testing.T) {
	auths := []*Auth{{ID: "higher"}, {ID: "target"}, {ID: "other"}}
	for _, tc := range []struct {
		name    string
		allowed []string
		want    []string
	}{
		{"target", []string{"target"}, []string{"target"}},
		{"no matching rule", nil, []string{"higher", "target", "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), routingPoolKey{}, &routingPool{decide: func(_ routing.RequestAttrs, eligible []string) routing.Decision {
				if !reflect.DeepEqual(eligible, []string{"higher", "target", "other"}) {
					t.Fatalf("eligible = %v", eligible)
				}
				return routing.Decision{Allowed: tc.allowed}
			}})
			var ids []string
			for _, auth := range narrowRoutingPool(ctx, auths) {
				ids = append(ids, auth.ID)
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("got %v, want %v", ids, tc.want)
			}
		})
	}
}

func TestRoutingAffinityPrecedesRule(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour})
	defer selector.Stop()
	auths := []*Auth{
		{ID: "high", Provider: "test", Status: StatusActive, Attributes: map[string]string{"priority": "10"}},
		{ID: "low", Provider: "test", Status: StatusActive, Attributes: map[string]string{"priority": "0"}},
	}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "session"}}
	ctx := context.Background()
	first, err := selector.Pick(ctx, "test", "model", opts, auths)
	if err != nil || first.ID != "high" {
		t.Fatalf("initial pick = %v, %v", first, err)
	}
	ctx = context.WithValue(ctx, routingPoolKey{}, &routingPool{decide: func(_ routing.RequestAttrs, _ []string) routing.Decision {
		return routing.Decision{Allowed: []string{"low"}}
	}})
	kept, err := selector.Pick(ctx, "test", "model", opts, auths)
	if err != nil || kept.ID != "high" {
		t.Fatalf("bound pick = %v, %v", kept, err)
	}
	newOpts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "new-session"}}
	selected, err := selector.Pick(ctx, "test", "model", newOpts, auths)
	if err != nil || selected.ID != "low" {
		t.Fatalf("new pick = %v, %v", selected, err)
	}
}

func TestRoutingPluginDelegatedWeightedUsesNarrowedPool(t *testing.T) {
	manager := NewManager(nil, &WeightedRoundRobinSelector{}, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "routing-plugin"})
	var candidates []*Auth
	for _, a := range []*Auth{
		{ID: "high", Provider: "routing-plugin", Status: StatusActive, Attributes: map[string]string{"priority": "10", "weight": "1"}},
		{ID: "zero", Provider: "routing-plugin", Status: StatusActive, Attributes: map[string]string{"weight": "0"}},
		{ID: "target", Provider: "routing-plugin", Status: StatusActive, Attributes: map[string]string{"weight": "1"}},
	} {
		if _, err := manager.Register(WithSkipPersist(context.Background()), a); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, a)
	}
	plugin := &fakePluginScheduler{handled: true, resp: pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin}}
	pool := &routingPool{decide: func(_ routing.RequestAttrs, ids []string) routing.Decision {
		for _, id := range ids {
			if id == "zero" {
				t.Fatal("zero-weight credential passed the rule gate")
			}
		}
		return routing.Decision{Allowed: []string{"target"}}
	}}
	ctx := context.WithValue(context.Background(), routingPoolKey{}, pool)
	selected, handled, err := manager.pickViaPluginScheduler(ctx, plugin, "routing-plugin", []string{"routing-plugin"}, "", cliproxyexecutor.Options{}, nil, candidates, candidates)
	if err != nil || !handled || selected == nil || selected.ID != "target" {
		t.Fatalf("pick = %v, handled %v, err %v", selected, handled, err)
	}
}

func TestDryRunDoesNotMoveAffinityOrSelectorState(t *testing.T) {
	provider, model := "routing-dry-run", "routing-dry-run-model"
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour})
	defer affinity.Stop()
	manager := NewManager(nil, affinity, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
	for _, id := range []string{"dry-a", "dry-b"} {
		if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: provider, Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	cacheKey := provider + "::header:session::" + canonicalModelKey(model)
	affinity.cache.Set(cacheKey, "dry-b")
	affinity.cache.mu.RLock()
	before := affinity.cache.entries[cacheKey]
	affinity.cache.mu.RUnlock()
	rr := affinity.fallback.(*RoundRobinSelector)
	rr.mu.Lock()
	beforeCursors := len(rr.lastPicked)
	rr.mu.Unlock()
	trace, err := manager.DryRunRoute(context.Background(), routing.DryRunRequest{Model: model, SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if trace.Pick < 0 || trace.Candidates[trace.Pick].ID != "dry-b" || trace.Affinity.State != routing.AffinityKept {
		t.Fatalf("trace = %+v", trace)
	}
	affinity.cache.mu.RLock()
	after := affinity.cache.entries[cacheKey]
	affinity.cache.mu.RUnlock()
	if before.authID != after.authID || !before.expiresAt.Equal(after.expiresAt) {
		t.Fatalf("affinity changed: %v -> %v", before, after)
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if len(rr.lastPicked) != beforeCursors {
		t.Fatal("dry run advanced round robin")
	}
}

func TestRoutingTraceRecordsRuleAndNoEligible(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "trace-test"})
	for _, a := range []*Auth{{ID: "trace-high", Provider: "trace-test", Status: StatusActive, Attributes: map[string]string{"priority": "2"}}, {ID: "trace-low", Provider: "trace-test", Status: StatusActive}} {
		if _, err := manager.Register(WithSkipPersist(context.Background()), a); err != nil {
			t.Fatal(err)
		}
	}
	ctx, holder := routing.WithTraceHolder(context.Background())
	ctx, capture := manager.beginRoutingCapture(ctx, []string{"trace-test"}, "", cliproxyexecutor.Options{}, nil)
	pool := &routingPool{decide: func(_ routing.RequestAttrs, _ []string) routing.Decision {
		return routing.Decision{Applied: 0, Allowed: []string{"trace-low"}, Matched: []routing.RuleMatch{{Index: 0, Targets: []string{"trace-low"}, Status: routing.RuleApplied}}}
	}}
	ctx = context.WithValue(ctx, routingPoolKey{}, pool)
	auths := []*Auth{{ID: "trace-high", Attributes: map[string]string{"priority": "2"}}, {ID: "trace-low"}}
	narrowed := narrowRoutingPool(ctx, auths)
	finishRoutingCapture(ctx, capture, narrowed[0], nil)
	trace := holder.Last()
	if trace.Pick != 1 || trace.Candidates[trace.Pick].ID != "trace-low" || trace.Rules == nil || trace.Rules.Applied != 0 || trace.Tier == nil || len(trace.Tier.Pool) != 1 || trace.Tier.Pool[0] != trace.Pick {
		t.Fatalf("trace = %+v", trace)
	}
	failedCtx, failed := manager.beginRoutingCapture(ctx, []string{"trace-test"}, "", cliproxyexecutor.Options{}, nil)
	// The previous capture belongs to one selection; start the next selection from the request context.
	if failed != nil {
		t.Fatal("nested capture should be reused")
	}
	_ = failedCtx
	failureCtx, failureHolder := routing.WithTraceHolder(context.Background())
	failureCtx, failure := manager.beginRoutingCapture(failureCtx, []string{"trace-test"}, "", cliproxyexecutor.Options{}, map[string]struct{}{"trace-high": {}, "trace-low": {}})
	finishRoutingCapture(failureCtx, failure, nil, &Error{Code: "auth_not_found"})
	if got := failureHolder.Last(); got.Error != "no_eligible" || got.Pick != -1 || got.Candidates[0].Gate != routing.GateTried {
		t.Fatalf("failure trace = %+v", got)
	}
}

func TestRoutingClientDetectionInNestedRules(t *testing.T) {
	yes := internalconfig.RoutingRule{When: &internalconfig.RoutingCondition{Not: &internalconfig.RoutingCondition{Any: []internalconfig.RoutingCondition{{Client: &internalconfig.RoutingMatch{Is: "codex"}}}}}}
	no := internalconfig.RoutingRule{When: &internalconfig.RoutingCondition{Model: &internalconfig.RoutingMatch{Is: "model"}}}
	if !routingRulesNeedClient([]internalconfig.RoutingRule{yes}) || routingRulesNeedClient([]internalconfig.RoutingRule{no}) {
		t.Fatal("incorrect client condition detection")
	}
	disabled := false
	yes.Enabled = &disabled
	if routingRulesNeedClient([]internalconfig.RoutingRule{yes}) {
		t.Fatal("disabled rule should not require client detection")
	}
}

func TestRoutingRulesFromYAMLApplyAcrossPriorityTiers(t *testing.T) {
	cfg, err := internalconfig.ParseConfigBytes([]byte(`routing:
  rules:
    - name: eligible fallback
      route-to: [rule-low]
    - name: unavailable target
      route-to: [rule-missing]
`))
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "yaml-routing"})
	for _, a := range []*Auth{{ID: "rule-high", Provider: "yaml-routing", Status: StatusActive, Attributes: map[string]string{"priority": "10"}}, {ID: "rule-low", Provider: "yaml-routing", Status: StatusActive}} {
		if _, err := manager.Register(WithSkipPersist(context.Background()), a); err != nil {
			t.Fatal(err)
		}
	}
	manager.SetConfig(cfg)
	ctx, holder := routing.WithTraceHolder(context.Background())
	selected, _, err := manager.pickNext(ctx, "yaml-routing", "", cliproxyexecutor.Options{}, nil)
	if err != nil || selected == nil || selected.ID != "rule-low" {
		t.Fatalf("selected=%v err=%v", selected, err)
	}
	trace := holder.Last()
	if trace == nil || trace.Rules == nil || trace.Rules.Applied != 0 || len(trace.Rules.Matched) != 2 || trace.Rules.Matched[1].Status != routing.RuleFellThrough {
		t.Fatalf("trace=%+v", trace)
	}
}

func TestDryRunSchedulerRoundRobinDoesNotAdvance(t *testing.T) {
	provider, model := "dry-scheduler", "dry-scheduler-model"
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
	for _, id := range []string{"scheduler-a", "scheduler-b"} {
		if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: provider, Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	first, _, err := manager.pickNext(context.Background(), provider, model, cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	trace, err := manager.DryRunRoute(context.Background(), routing.DryRunRequest{Model: model})
	if err != nil || !trace.PickPredicted || trace.Pick < 0 {
		t.Fatalf("trace=%+v err=%v", trace, err)
	}
	predicted := trace.Candidates[trace.Pick].ID
	next, _, err := manager.pickNext(context.Background(), provider, model, cliproxyexecutor.Options{}, nil)
	if err != nil || next.ID != predicted || next.ID == first.ID {
		t.Fatalf("first=%v predicted=%q next=%v err=%v", first, predicted, next, err)
	}
}

func TestRoutingAffinitySurvivesRulesReloadAndTracesFailover(t *testing.T) {
	provider := "affinity-rules"
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour})
	defer affinity.Stop()
	manager := NewManager(nil, affinity, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
	for _, id := range []string{"affinity-a", "affinity-b"} {
		if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: provider, Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, holder := routing.WithTraceHolder(context.Background())
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "session"}}
	first, _, err := manager.pickNext(ctx, provider, "", opts, nil)
	if err != nil || first.ID != "affinity-a" {
		t.Fatalf("initial=%v err=%v", first, err)
	}
	cfg, err := internalconfig.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [affinity-b]\n"))
	if err != nil {
		t.Fatal(err)
	}
	manager.SetConfig(cfg)
	kept, _, err := manager.pickNext(ctx, provider, "", opts, nil)
	if err != nil || kept.ID != "affinity-a" || holder.Last().Affinity.State != routing.AffinityKept || holder.Last().DecidedBy != routing.DecidedByAffinity {
		t.Fatalf("kept=%v trace=%+v err=%v", kept, holder.Last(), err)
	}
	failover, _, err := manager.pickNext(ctx, provider, "", opts, map[string]struct{}{"affinity-a": {}})
	trace := holder.Last()
	if err != nil || failover.ID != "affinity-b" || trace.Affinity.State != routing.AffinityBroken || trace.Candidates[trace.Affinity.Bound].Gate != routing.GateTried || trace.Selection != 2 {
		t.Fatalf("failover=%v trace=%+v err=%v", failover, trace, err)
	}
}

func TestRoutingWeightedAndEmptyIntersectionSelection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules string
		want  string
	}{
		{"zero weight falls through", "    - route-to: [positive-low]\n    - route-to: [zero-low]\n", "positive-low"},
		{"empty intersection preserves pool", "    - route-to: [absent]\n", "positive-high"},
		{"unmatched rule preserves pool", "    - when: { model: { is: other } }\n      route-to: [positive-low]\n", "positive-high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := internalconfig.ParseConfigBytes([]byte("routing:\n  rules:\n" + tc.rules))
			if err != nil {
				t.Fatal(err)
			}
			for _, legacy := range []bool{false, true} {
				manager := NewManager(nil, &WeightedRoundRobinSelector{}, nil)
				manager.RegisterExecutor(schedulerTestExecutor{provider: "weighted-rule"})
				for _, a := range []*Auth{
					{ID: "positive-high", Provider: "weighted-rule", Status: StatusActive, Attributes: map[string]string{"priority": "10", "weight": "1"}},
					{ID: "positive-low", Provider: "weighted-rule", Status: StatusActive, Attributes: map[string]string{"weight": "1"}},
					{ID: "zero-low", Provider: "weighted-rule", Status: StatusActive, Attributes: map[string]string{"weight": "0"}},
				} {
					if _, err := manager.Register(WithSkipPersist(context.Background()), a); err != nil {
						t.Fatal(err)
					}
				}
				manager.SetConfig(cfg)
				var picked *Auth
				if legacy {
					picked, _, err = manager.pickNextLegacy(context.Background(), "weighted-rule", "", cliproxyexecutor.Options{}, nil)
				} else {
					picked, _, err = manager.pickNext(context.Background(), "weighted-rule", "", cliproxyexecutor.Options{}, nil)
				}
				if err != nil || picked == nil || picked.ID != tc.want {
					t.Fatalf("legacy %v: picked=%v err=%v want=%s", legacy, picked, err, tc.want)
				}
			}
		})
	}
}

func TestRoutingTraceSeparatesRetryLimitAndTried(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetRetryConfig(3, 0, 0)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "trace-retry"})
	for _, a := range []*Auth{{ID: "limited", Provider: "trace-retry", Status: StatusActive, Metadata: map[string]any{"request_retry": 0}}, {ID: "tried", Provider: "trace-retry", Status: StatusActive, Metadata: map[string]any{"request_retry": 3}}} {
		if _, err := manager.Register(WithSkipPersist(context.Background()), a); err != nil {
			t.Fatal(err)
		}
	}
	opts := withHomeRetryRound(cliproxyexecutor.Options{}, 1)
	base := context.Background()
	_, none := manager.beginRoutingCapture(base, []string{"trace-retry"}, "", opts, nil)
	if none != nil {
		t.Fatal("capture allocated without holder")
	}
	ctx, holder := routing.WithTraceHolder(base)
	_, _, _ = manager.pickNext(ctx, "trace-retry", "", opts, map[string]struct{}{"limited": {}, "tried": {}})
	trace := holder.Last()
	if trace == nil || len(trace.Candidates) != 2 || trace.Candidates[0].Gate != routing.GateRetryLimit || trace.Candidates[1].Gate != routing.GateTried || trace.RetryRound != 1 {
		t.Fatalf("trace=%+v", trace)
	}
}

func TestRoutingTraceHeldBindingOnTransientFailover(t *testing.T) {
	provider, model := "trace-held", "trace-held-model"
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour, RebindOn: SessionAffinityRebindOnQuota})
	defer affinity.Stop()
	manager := NewManager(nil, affinity, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
	for _, a := range []*Auth{{ID: "held-high", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "10"}}, {ID: "held-low", Provider: provider, Status: StatusActive}} {
		if _, err := manager.Register(WithSkipPersist(context.Background()), a); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(a.ID, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	}
	ctx, holder := routing.WithTraceHolder(context.Background())
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "session"}}
	first, _, err := manager.pickNext(ctx, provider, model, opts, nil)
	if err != nil || first.ID != "held-high" {
		t.Fatalf("first=%v err=%v", first, err)
	}
	manager.MarkResult(ctx, Result{AuthID: first.ID, Provider: provider, Model: model, Success: false, Error: &Error{HTTPStatus: 503, Message: "temporary"}, Options: opts})
	next, _, err := manager.pickNext(ctx, provider, model, opts, nil)
	trace := holder.Last()
	if err != nil || next == nil || next.ID != "held-low" || trace.Affinity.State != routing.AffinityHeld || trace.Candidates[trace.Affinity.Bound].ID != "held-high" {
		t.Fatalf("next=%v trace=%+v err=%v", next, trace, err)
	}
}

func TestRoutingMixedProvidersRuleTargetBeatsHigherTier(t *testing.T) {
	cfg, err := internalconfig.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [mixed-low]\n"))
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	for _, provider := range []string{"mixed-high-provider", "mixed-low-provider"} {
		manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
	}
	for _, a := range []*Auth{{ID: "mixed-high", Provider: "mixed-high-provider", Status: StatusActive, Attributes: map[string]string{"priority": "10"}}, {ID: "mixed-low", Provider: "mixed-low-provider", Status: StatusActive}} {
		if _, err := manager.Register(WithSkipPersist(context.Background()), a); err != nil {
			t.Fatal(err)
		}
	}
	manager.SetConfig(cfg)
	for _, legacy := range []bool{false, true} {
		var selected *Auth
		if legacy {
			selected, _, _, err = manager.pickNextMixedLegacy(context.Background(), []string{"mixed-high-provider", "mixed-low-provider"}, "", cliproxyexecutor.Options{}, nil)
		} else {
			selected, _, _, err = manager.pickNextMixed(context.Background(), []string{"mixed-high-provider", "mixed-low-provider"}, "", cliproxyexecutor.Options{}, nil)
		}
		if err != nil || selected == nil || selected.ID != "mixed-low" {
			t.Fatalf("legacy=%v selected=%v err=%v", legacy, selected, err)
		}
	}
}

type routingTraceExecutor struct {
	schedulerTestExecutor
	prepared *routing.Trace
	executed *routing.Trace
}

func (e *routingTraceExecutor) ShouldPrepareRequestAuth(*Auth) bool { return true }
func (e *routingTraceExecutor) PrepareRequestAuth(ctx context.Context, a *Auth) (*Auth, error) {
	e.prepared = routing.TraceFromContext(ctx)
	return a, nil
}
func (e *routingTraceExecutor) Execute(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.executed = routing.TraceFromContext(ctx)
	return cliproxyexecutor.Response{}, nil
}

func TestRoutingTraceAttachedBeforePreparationAndExecution(t *testing.T) {
	provider, model := "trace-execute", "trace-execute-model"
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	executor := &routingTraceExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: provider}}
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: "execute-auth", Provider: provider, Status: StatusActive}); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient("execute-auth", provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("execute-auth") })
	ctx, holder := routing.WithTraceHolder(context.Background())
	if _, err := manager.Execute(ctx, []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err != nil {
		t.Fatal(err)
	}
	if executor.prepared == nil || executor.prepared != executor.executed || executor.executed != holder.Last() || executor.executed.Candidates[executor.executed.Pick].ID != "execute-auth" {
		t.Fatalf("prepared=%p executed=%p holder=%p", executor.prepared, executor.executed, holder.Last())
	}
}
