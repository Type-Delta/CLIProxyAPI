package auth

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func telemetryPool() []*Auth {
	at := time.Date(2090, 1, 1, 0, 0, 0, 0, time.UTC)
	return []*Auth{
		{ID: "a", Provider: "zai", Status: StatusActive, Attributes: map[string]string{"weight": "1", "gsr_tier": "pro"}, Quota: QuotaState{Signals: map[string]string{"weekly_reset_at": at.Format(time.RFC3339)}}},
		{ID: "b", Provider: "zai", Status: StatusActive, Attributes: map[string]string{"weight": "2"}, Quota: QuotaState{Signals: map[string]string{"weekly_reset_at": at.Add(24 * time.Hour).Format(time.RFC3339)}}},
		{ID: "c", Provider: "zai", Status: StatusActive, Attributes: map[string]string{"weight": "3"}, Quota: QuotaState{Signals: map[string]string{"weekly_reset_at": at.Add(48 * time.Hour).Format(time.RFC3339)}}},
	}
}
func telemetryCapture() (context.Context, *routingCapture) {
	c := &routingCapture{trace: &routing.Trace{Version: 1, Mode: routing.ModeLocal, Pick: -1, Affinity: &routing.AffinityStep{State: routing.AffinityNone, Bound: -1}}, indexes: map[string]int{}}
	return context.WithValue(context.Background(), routingCaptureKey{}, c), c
}
func TestRoutingGSRFactorsMatchBothPickers(t *testing.T) {
	pool := telemetryPool()
	now := time.Now()
	factors, err := greedySoonestRecoveryFactors(pool, "zai", "", now, defaultGreedySoonestRecoveryHalfLife)
	if err != nil {
		t.Fatal(err)
	}
	_, weights, err := greedySoonestRecoveryWeights(pool, "zai", "", now, defaultGreedySoonestRecoveryHalfLife)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := newAuthScheduler(&GreedySoonestRecoverySelector{})
	scheduler.rebuild(pool)
	shard := scheduler.providers["zai"].ensureModelLocked("", now)
	_, scheduled, err := greedySoonestRecoveryScheduledWeights(shard.readyByPriority[0].all.flat, nil, "", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range pool {
		f := factors[a.ID]
		if f.Weight != weights[a.ID] || f.Weight != scheduled[a.ID] || f.BaseWeight != greedySoonestRecoveryWeight(time.Duration(f.DelayMs)*time.Millisecond, defaultGreedySoonestRecoveryHalfLife) {
			t.Fatalf("factor drift: %+v %v %v", f, weights, scheduled)
		}
		if math.Abs(f.Decay-math.Exp(-float64(f.DelayMs)/float64(defaultGreedySoonestRecoveryHalfLife.Milliseconds()))) > 1e-12 {
			t.Fatalf("decay %+v", f)
		}
		if f.RecoverySource != "signal:weekly_reset_at" {
			t.Fatalf("source %+v", f)
		}
	}
	if factors["a"].TierSource != "override" || factors["a"].TierMultiplier != 4 {
		t.Fatalf("tier %+v", factors["a"])
	}
}

func TestRoutingLiveStrategySteps(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selector Selector
		fallback bool
	}{
		{routing.StrategyRoundRobin, &RoundRobinSelector{}, false},
		{routing.StrategyFillFirst, &FillFirstSelector{}, false},
		{routing.StrategyWeightedRoundRobin, &WeightedRoundRobinSelector{}, false},
		{routing.StrategyGreedySoonestRecovery, &GreedySoonestRecoverySelector{}, false},
		{"gsr-fallback", &GreedySoonestRecoverySelector{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, schedulerPath := range []bool{false, true} {
				pool := telemetryPool()
				if tc.fallback {
					pool[1].Quota = QuotaState{}
				}
				ctx, c := telemetryCapture()
				var picked *Auth
				var err error
				if schedulerPath {
					s := newAuthScheduler(tc.selector)
					s.rebuild(pool)
					picked, err = s.pickSingle(ctx, "zai", "", cliproxyexecutor.Options{}, nil)
				} else {
					picked, err = tc.selector.Pick(ctx, "zai", "", cliproxyexecutor.Options{}, pool)
				}
				if err != nil {
					t.Fatal(err)
				}
				step := c.trace.StrategyStep
				if step == nil || len(step.Weights) != 3 || c.trace.Candidates[step.Wanted].ID != picked.ID {
					t.Fatalf("scheduler=%t pick=%v step=%+v", schedulerPath, picked, step)
				}
				if tc.fallback && (step.Applied != routing.StrategyRoundRobin || step.Fallback != "missing_recovery_data") {
					t.Fatalf("fallback %+v", step)
				}
				sum := 0.0
				for _, w := range step.Weights {
					sum += w.Share
				}
				if math.Abs(sum-1) > 1e-12 {
					t.Fatalf("shares %+v", step)
				}
			}
		})
	}
}

func TestRoutingDryRunWeightedPredictionAndState(t *testing.T) {
	for _, gsr := range []bool{false, true} {
		for _, legacy := range []bool{false, true} {
			t.Run(map[bool]string{false: "wrr", true: "gsr"}[gsr]+map[bool]string{false: "-scheduler", true: "-legacy"}[legacy], func(t *testing.T) {
				var selector Selector = &WeightedRoundRobinSelector{}
				if gsr {
					selector = &GreedySoonestRecoverySelector{}
				}
				configured := selector
				if legacy {
					aff := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: selector, TTL: time.Hour})
					defer aff.Stop()
					configured = aff
				}
				m := NewManager(nil, configured, nil)
				m.RegisterExecutor(schedulerTestExecutor{provider: "zai"})
				model := "telemetry-prediction"
				pool := telemetryPool()
				for _, a := range pool {
					if _, err := m.Register(WithSkipPersist(context.Background()), a); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(a.ID, "zai", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
				}
				m.scheduler.rebuild(pool)
				pick := func() *Auth {
					a, _, err := m.pickNext(context.Background(), "zai", model, cliproxyexecutor.Options{}, nil)
					if err != nil {
						t.Fatal(err)
					}
					return a
				}
				pick()
				snapshot := func() []byte {
					var v any
					if legacy || gsr {
						switch s := selector.(type) {
						case *WeightedRoundRobinSelector:
							v = s.states
						case *GreedySoonestRecoverySelector:
							v = s.states
						}
					} else {
						shard := m.scheduler.providers["zai"].modelShards[model]
						v = snapshotReadyViewCursors(shard.readyByPriority[0].all)
					}
					// Unexported state fields require explicit map snapshots.
					switch state := v.(type) {
					case map[string]*smoothWeightedState:
						out := map[string]any{}
						for k, s := range state {
							out[k] = []any{s.current, s.weights}
						}
						v = out
					case readyViewCursorState:
						v = []any{state.lastPicked, state.weightedState.current, state.weightedState.weights, state.gsrState.current, state.gsrState.weights}
					}
					b, _ := json.Marshal(v)
					return b
				}
				before := string(snapshot())
				trace, err := m.DryRunRoute(context.Background(), routing.DryRunRequest{Model: model})
				if err != nil {
					t.Fatal(err)
				}
				if before != string(snapshot()) {
					t.Fatal("dry run mutated live state")
				}
				if trace.StrategyStep == nil || len(trace.StrategyStep.Next) != 10 || !trace.PickPredicted {
					t.Fatalf("unpredicted %+v", trace)
				}
				next := pick()
				if trace.Candidates[trace.Pick].ID != next.ID {
					t.Fatalf("dry=%s live=%s", trace.Candidates[trace.Pick].ID, next.ID)
				}
				if gsr && !legacy {
					encoded, _ := json.Marshal(trace)
					t.Logf("GSR dry-run fixture: %s", encoded)
				}
			})
		}
	}
}

func TestRoutingAffinityBindingProvenance(t *testing.T) {
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour, RebindOn: SessionAffinityRebindOnQuota})
	defer s.Stop()
	pool := telemetryPool()
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"provenance"}}}
	firstID := "0123456789abcdef0123456789abcdef"
	ctx := coreusage.WithProxyRequestID(context.Background(), firstID)
	first, err := s.Pick(ctx, "zai", "", opts, pool)
	if err != nil {
		t.Fatal(err)
	}
	key := "zai::header:provenance::"
	id, at, request, ok := s.cache.LookupBinding(key)
	if !ok || id != first.ID || at.IsZero() || request != firstID {
		t.Fatalf("binding %s %v %s %t", id, at, request, ok)
	}
	ctx, c := telemetryCapture()
	ctx = coreusage.WithProxyRequestID(ctx, "fedcba9876543210fedcba9876543210")
	_, err = s.Pick(ctx, "zai", "", opts, pool)
	if err != nil {
		t.Fatal(err)
	}
	if c.trace.StrategyStep != nil || c.trace.Affinity.BoundRequestID != firstID || c.trace.Affinity.BoundAtMs != at.UnixMilli() {
		t.Fatalf("kept %+v", c.trace)
	}
	// Held failover keeps the original binding and provenance.
	first.Unavailable = true
	first.NextRetryAfter = time.Now().Add(time.Hour)
	ctx, c = telemetryCapture()
	ctx = withSessionAffinityTransientAuthIDs(ctx, map[string]struct{}{first.ID: {}})
	_, err = s.Pick(ctx, "zai", "", opts, pool)
	if err != nil {
		t.Fatal(err)
	}
	_, after, request, _ := s.cache.LookupBinding(key)
	if c.trace.Affinity.State != routing.AffinityHeld || !after.Equal(at) || request != firstID {
		t.Fatalf("held %+v", c.trace.Affinity)
	}
	s.rebindOnQuotaOnly = false
	ctx, c = telemetryCapture()
	ctx = coreusage.WithProxyRequestID(ctx, "fedcba9876543210fedcba9876543210")
	rebound, err := s.Pick(ctx, "zai", "", opts, pool)
	if err != nil {
		t.Fatal(err)
	}
	_, after, request, _ = s.cache.LookupBinding(key)
	if rebound.ID == first.ID || request == firstID || after.Before(at) {
		t.Fatalf("rebound %s %v %s", rebound.ID, after, request)
	}
	s.cache.SetAliases(rebound.ID, key, "alias")
	_, aliasAt, aliasRequest, _ := s.cache.LookupBinding("alias")
	if !reflect.DeepEqual(aliasAt, after) || aliasRequest != request {
		t.Fatal("alias merge lost provenance")
	}
}

func TestRoutingMixedSchedulerSimulationParity(t *testing.T) {
	for _, selector := range []Selector{&RoundRobinSelector{}, &FillFirstSelector{}, &WeightedRoundRobinSelector{}, &GreedySoonestRecoverySelector{}} {
		for _, fallback := range []bool{false, true} {
			pool := telemetryPool()
			pool[1].Provider = "codex"
			// Use credential fields so recovery math is independent of the provider signal family.
			for i, a := range pool {
				a.Quota = QuotaState{NextRecoverAt: time.Date(2090, 1, 1+i, 0, 0, 0, 0, time.UTC)}
			}
			if fallback {
				pool[1].Quota = QuotaState{}
			}
			s := newAuthScheduler(selector)
			s.rebuild(pool)
			providers := []string{"codex", "zai"}
			shards := []*modelScheduler{s.providers["codex"].ensureModelLocked("", time.Now()), s.providers["zai"].ensureModelLocked("", time.Now())}
			ctx, c := telemetryCapture()
			// Advance real cursors and credits before copying them.
			if _, _, err := s.pickMixed(ctx, providers, "", cliproxyexecutor.Options{}, nil); err != nil {
				t.Fatal(err)
			}
			_, c = telemetryCapture()
			step, sim := s.scheduledStrategySimulation(c, shards, false, 0, nil, s.strategy, providers, "")
			for range 10 {
				predicted := sim.next()
				live, _, err := s.pickMixed(ctx, providers, "", cliproxyexecutor.Options{}, nil)
				if err != nil || predicted == nil || live.ID != predicted.ID {
					t.Fatalf("%T fallback=%t predicted=%v live=%v err=%v step=%+v", selector, fallback, predicted, live, err, step)
				}
			}
		}
	}
}

func TestRoutingRecoveryAndTierSources(t *testing.T) {
	now := time.Now()
	a := &Auth{ID: "a", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}}
	a.NextRetryAfter = now.Add(time.Hour)
	a.Quota.NextRecoverAt = now.Add(2 * time.Hour)
	if _, source, _ := authRecoveryTimestampWithSourceAt(a, "codex", "m", now); source != "retry_after" {
		t.Fatal(source)
	}
	a.NextRetryAfter = time.Time{}
	if _, source, _ := authRecoveryTimestampWithSourceAt(a, "codex", "m", now); source != "quota_recover_at" {
		t.Fatal(source)
	}
	a.ModelStates = map[string]*ModelState{"m": {NextRetryAfter: now.Add(time.Minute)}}
	if _, source, _ := authRecoveryTimestampWithSourceAt(a, "codex", "m", now); source != "model_retry_after" {
		t.Fatal(source)
	}
	a.ModelStates["m"].Quota.NextRecoverAt = now.Add(time.Second)
	if _, source, _ := authRecoveryTimestampWithSourceAt(a, "codex", "m", now); source != "model_quota_recover_at" {
		t.Fatal(source)
	}
	a.Quota.ManualReset = ManualResetObservation{Provider: "codex", ObservedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Millisecond)}
	if _, source, _ := authRecoveryTimestampWithSourceAt(a, "codex", "m", now); source != "manual_reset" {
		t.Fatal(source)
	}
	if _, source := GSRAllowanceTierForAuthWithSource(a, "codex"); source != "plan" {
		t.Fatal(source)
	}
	a.Attributes = nil
	a.Quota.Signals = map[string]string{"x-codex-plan-type": "pro"}
	if _, source := GSRAllowanceTierForAuthWithSource(a, "codex"); source != "signal" {
		t.Fatal(source)
	}
}

func TestRoutingDryRunGSRMissingCacheNeverRefreshes(t *testing.T) {
	m := NewManager(nil, &GreedySoonestRecoverySelector{}, nil)
	m.RegisterExecutor(schedulerTestExecutor{provider: "zai"})
	model := "dry-run-no-network"
	a := &Auth{ID: "dry-no-network", Provider: "zai", Status: StatusActive}
	if _, err := m.Register(WithSkipPersist(context.Background()), a); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, "zai", []*registry.ModelInfo{{ID: model}})
	defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
	m.SetQuotaRecoveryRefresher(func(context.Context, *Auth, string) error {
		t.Fatal("dry run refreshed quota over the network")
		return nil
	})
	trace, err := m.DryRunRoute(context.Background(), routing.DryRunRequest{Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if trace.PickPredicted || trace.StrategyStep == nil || trace.StrategyStep.Applied != routing.StrategyRoundRobin || trace.StrategyStep.Fallback != routing.FallbackMissingRecoveryDataRefreshPending {
		t.Fatalf("trace %+v", trace)
	}
}
