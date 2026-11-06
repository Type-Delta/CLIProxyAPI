package auth

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
	usage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	trans "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestRoutingRegressionLCPForkProvenance(t *testing.T) {
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour})
	defer s.Stop()
	pool := []*Auth{{ID: "a", Provider: "openai"}, {ID: "b", Provider: "openai"}}
	request := func(body string, requestID string) (ex.Options, *routingCapture) {
		opts := ex.Options{SourceFormat: trans.FormatOpenAI, OriginalRequest: []byte(body), Metadata: map[string]any{ex.CallerScopeMetadataKey: "review-caller"}}
		ctx, c := telemetryCapture()
		ctx = usage.WithProxyRequestID(ctx, requestID)
		if _, err := s.Pick(ctx, "openai", "model", opts, pool); err != nil {
			t.Fatal(err)
		}
		return opts, c
	}
	parent, _ := request(`{"messages":[{"role":"system","content":"stable"},{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":"second"}]}`, "11111111111111111111111111111111")
	parentKey := lcpAffinityNamespace("openai", "model", parent.Metadata) + "::" + parent.Metadata[ex.LCPAffinitySessionIDMetadataKey].(string)
	_, parentAt, _, _ := s.cache.LookupBinding(parentKey)
	opts, c := request(`{"messages":[{"role":"system","content":"stable"},{"role":"user","content":"first"},{"role":"assistant","content":"different"},{"role":"user","content":"fork"}]}`, "22222222222222222222222222222222")
	if opts.Metadata[ex.IsForkMetadataKey] != true {
		t.Fatalf("expected fork: %+v", opts.Metadata)
	}
	id := opts.Metadata[ex.LCPAffinitySessionIDMetadataKey].(string)
	key := lcpAffinityNamespace("openai", "model", opts.Metadata) + "::" + id
	_, at, rid, ok := s.cache.LookupBinding(key)
	if !ok || at.IsZero() || !at.Equal(parentAt) || rid != "11111111111111111111111111111111" || c.trace.Affinity.BoundRequestID != rid {
		t.Fatalf("fork exists, session=%s cache-present=%v bound-at=%v request=%q trace-affinity=%+v", id, ok, at, rid, c.trace.Affinity)
	}
}
func TestRoutingRegressionDryRunWebsocket(t *testing.T) {
	for _, provider := range []string{"codex", "xai"} {
		for _, selector := range []Selector{&RoundRobinSelector{}, &WeightedRoundRobinSelector{}, &FillFirstSelector{}} {
			t.Run(fmt.Sprintf("%s/%T", provider, selector), func(t *testing.T) {
				m := NewManager(nil, selector, nil)
				m.RegisterExecutor(schedulerTestExecutor{provider: provider})
				model := "review-websocket"
				for _, a := range []*Auth{
					{ID: "a-http", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "10"}},
					{ID: "b-ws", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "0", "websockets": "true"}},
					{ID: "c-ws", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "0", "websockets": "true"}},
				} {
					if _, err := m.Register(WithSkipPersist(context.Background()), a); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(a.ID, provider, []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
				}
				ctx := ex.WithDownstreamWebsocket(context.Background())
				if _, _, err := m.pickNext(ctx, provider, model, ex.Options{}, nil); err != nil {
					t.Fatal(err)
				}
				if _, _, err := m.pickNext(context.Background(), provider, model, ex.Options{}, nil); err != nil {
					t.Fatal(err)
				}
				trace, err := m.DryRunRoute(context.Background(), routing.DryRunRequest{Model: model, DownstreamWebsocket: true})
				if err != nil {
					t.Fatal(err)
				}
				live, _, err := m.pickNext(ctx, provider, model, ex.Options{}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if !trace.PickPredicted || trace.Candidates[trace.Pick].ID != live.ID || trace.Tier.Priority != 0 || len(trace.Tier.Pool) != 2 {
					t.Fatalf("dry=%s tier=%+v live=%s", trace.Candidates[trace.Pick].ID, trace.Tier, live.ID)
				}
			})
		}
	}
}
func TestRoutingRegressionDryRunGSRRefreshParity(t *testing.T) {
	m := NewManager(nil, &GreedySoonestRecoverySelector{}, nil)
	m.RegisterExecutor(schedulerTestExecutor{provider: "zai"})
	model := "review-gsr-refresh"
	for _, id := range []string{"a", "b"} {
		if _, err := m.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: "zai", Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "zai", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	var calls atomic.Int32
	m.SetQuotaRecoveryRefresher(func(ctx context.Context, a *Auth, _ string) error {
		calls.Add(1)
		a.Quota.NextRecoverAt = time.Date(2090, 1, 1, 0, 0, 0, 0, time.UTC)
		if a.ID == "a" {
			a.Quota.NextRecoverAt = a.Quota.NextRecoverAt.Add(100 * time.Hour)
		}
		_, err := m.Update(WithSkipPersist(ctx), a)
		return err
	})
	trace, err := m.DryRunRoute(context.Background(), routing.DryRunRequest{Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("dry refreshed")
	}
	live, _, err := m.pickNext(context.Background(), "zai", model, ex.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || live.ID != "b" || trace.PickPredicted || trace.StrategyStep.Fallback != routing.FallbackMissingRecoveryDataRefreshPending || trace.StrategyStep.Wanted < 0 || len(trace.StrategyStep.Weights) != 2 {
		t.Fatalf("dry=%+v live=%s refreshes=%d", trace, live.ID, calls.Load())
	}
}

func routingBaselineGSRWeights(auths []*Auth, provider, model string, now time.Time, halfLife time.Duration) ([]*Auth, map[string]int64, error) {
	if len(auths) == 0 {
		return nil, nil, greedySoonestRecoveryTimestampError(provider, model)
	}
	deadlines := make(map[string]time.Time, len(auths))
	weighted := make([]*Auth, 0, len(auths))
	eligibleCount := 0
	var earliest time.Time
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		eligibleCount++
		deadline, ok := authRecoveryTimestampAt(auth, provider, model, now)
		if !ok {
			continue
		}
		deadlines[auth.ID] = deadline
		weighted = append(weighted, auth)
		if earliest.IsZero() || deadline.Before(earliest) {
			earliest = deadline
		}
	}
	if len(weighted) == 0 || len(weighted) != eligibleCount || earliest.IsZero() {
		return nil, nil, greedySoonestRecoveryTimestampError(provider, model)
	}
	if halfLife <= 0 {
		halfLife = defaultGreedySoonestRecoveryHalfLife
	}
	weights := make(map[string]int64, len(weighted))
	for _, auth := range weighted {
		if auth == nil {
			continue
		}
		baseWeight := greedySoonestRecoveryWeight(deadlines[auth.ID].Sub(earliest), halfLife)
		tierProvider := QuotaRecoveryProviderForAuth(auth)
		weights[auth.ID] = multiplyGSRWeight(baseWeight, gsrAllowanceMultiplier(GSRAllowanceTierForAuth(auth, tierProvider))) + authWeight(auth)
	}
	return weighted, weights, nil
}

func BenchmarkRoutingRegressionGSRNoTrace(b *testing.B) {
	pool := telemetryPool()
	now := time.Now()
	b.Run("before", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, routingRegressionWeights, _ = routingBaselineGSRWeights(pool, "zai", "", now, defaultGreedySoonestRecoveryHalfLife)
		}
	})
	b.Run("after", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, routingRegressionWeights, _ = greedySoonestRecoveryWeights(pool, "zai", "", now, defaultGreedySoonestRecoveryHalfLife)
		}
	})
}

var routingRegressionWeights map[string]int64

func TestRoutingRegressionGSRPreparedFactorsAfterExpiry(t *testing.T) {
	now := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	pool := telemetryPool()
	for _, a := range pool {
		a.Quota = QuotaState{NextRecoverAt: now.Add(time.Second)}
	}
	ctx, capture := telemetryCapture()
	factors := routingGSRFactors(ctx, len(pool))
	weighted, weights, err := greedySoonestRecoveryWeightsWithFactors(pool, "zai", "", now, defaultGreedySoonestRecoveryHalfLife, factors)
	if err != nil {
		t.Fatal(err)
	}
	state := &smoothWeightedState{}
	state.prepare(weights)
	captureStrategyPick(ctx, routing.StrategyGreedySoonestRecovery, weighted, "", defaultGreedySoonestRecoveryHalfLife, "", state, nil, factors)
	picked := pickSmoothWeightedAuthWithWeights(weighted, weights, state.current)
	step := capture.trace.StrategyStep
	if step.Applied != routing.StrategyGreedySoonestRecovery || step.Fallback != "" || capture.trace.Candidates[step.Wanted].ID != picked.ID {
		t.Fatalf("pick=%s step=%+v", picked.ID, step)
	}
	for _, w := range step.Weights {
		id := capture.trace.Candidates[w.Index].ID
		if w.Weight != weights[id] || w.RecoveryAtMs != now.Add(time.Second).UnixMilli() {
			t.Fatalf("factor drift: %+v", w)
		}
	}
}

func TestRoutingRegressionGSRNoCaptureAllocations(t *testing.T) {
	pool := telemetryPool()
	now := time.Now()
	before := testing.AllocsPerRun(100, func() {
		_, routingRegressionWeights, _ = routingBaselineGSRWeights(pool, "zai", "", now, defaultGreedySoonestRecoveryHalfLife)
	})
	after := testing.AllocsPerRun(100, func() {
		_, routingRegressionWeights, _ = greedySoonestRecoveryWeights(pool, "zai", "", now, defaultGreedySoonestRecoveryHalfLife)
	})
	if after != before {
		t.Fatalf("no-capture allocations = %g, before explanations = %g", after, before)
	}
	t.Logf("no-capture allocations: before=%g after=%g", before, after)
}
