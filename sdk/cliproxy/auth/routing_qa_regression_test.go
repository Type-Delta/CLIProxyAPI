package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cfg "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	trans "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestQAGSRFallsThroughAfterRefresh(t *testing.T) {
	for _, affinityOn := range []bool{false, true} {
		for _, mixed := range []bool{false, true} {
			for _, priorRule := range []bool{false, true} {
				t.Run(fmt.Sprintf("affinity=%v/mixed=%v/priorRule=%v", affinityOn, mixed, priorRule), func(t *testing.T) {
					const provider = "qa-gsr"
					const model = "qa-gsr-model"
					var selector Selector = &GreedySoonestRecoverySelector{}
					if affinityOn {
						a := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: selector, TTL: time.Hour})
						defer a.Stop()
						selector = a
					}
					m := NewManager(nil, selector, nil)
					m.RegisterExecutor(schedulerTestExecutor{provider: provider})
					for _, id := range []string{"qa-a", "qa-b", "qa-outside"} {
						if _, err := m.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: provider, Status: StatusActive}); err != nil {
							t.Fatal(err)
						}
						registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					}
					raw := "routing:\n  rules:\n"
					if priorRule {
						raw += "    - route-to: [qa-outside]\n"
					}
					raw += "    - route-to: [qa-a, qa-b]\n"
					c, err := cfg.ParseConfigBytes([]byte(raw))
					if err != nil {
						t.Fatal(err)
					}
					m.SetConfig(c)
					m.SetQuotaRecoveryRefresher(func(ctx context.Context, a *Auth, _ string) error {
						a.Disabled = true
						_, e := m.Update(WithSkipPersist(ctx), a)
						return e
					})
					ctx, h := routing.WithTraceHolder(context.Background())
					var got *Auth
					if mixed {
						got, _, _, err = m.pickNextMixed(ctx, []string{provider}, model, ex.Options{}, nil)
					} else {
						got, _, err = m.pickNext(ctx, provider, model, ex.Options{}, nil)
					}
					if err != nil || got == nil || got.ID != "qa-outside" {
						t.Fatalf("want healthy qa-outside after fall-through; got=%v err=%v trace=%+v", got, err, h.Last())
					}
					trace := h.Last()
					if trace.Rules == nil || len(trace.Rules.Matched) == 0 || trace.Rules.Matched[len(trace.Rules.Matched)-1].Status != routing.RuleFellThrough {
						t.Fatalf("wrong rule fall-through trace: %+v", trace)
					}
					if priorRule && trace.Rules.Applied != 0 {
						t.Fatalf("prior rule was not applied: %+v", trace.Rules)
					}
				})
			}
		}
	}
}

func TestQAGSRTraceRevalidatesGate(t *testing.T) {
	const provider = "qa-trace"
	const model = "qa-trace-model"
	m := NewManager(nil, &GreedySoonestRecoverySelector{}, nil)
	m.RegisterExecutor(schedulerTestExecutor{provider: provider})
	for _, id := range []string{"qa-a", "qa-b"} {
		m.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: provider, Status: StatusActive})
		registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	c, _ := cfg.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [qa-a,qa-b]\n"))
	m.SetConfig(c)
	m.SetQuotaRecoveryRefresher(func(ctx context.Context, a *Auth, _ string) error {
		if a.ID == "qa-a" {
			a.Disabled = true
			_, e := m.Update(WithSkipPersist(ctx), a)
			return e
		}
		return nil
	})
	ctx, h := routing.WithTraceHolder(context.Background())
	got, _, err := m.pickNext(ctx, provider, model, ex.Options{}, nil)
	if err != nil || got.ID != "qa-b" {
		t.Fatalf("got=%v err=%v", got, err)
	}
	for _, a := range h.Last().Candidates {
		if a.ID == "qa-a" && a.Gate != routing.GateDisabled {
			t.Fatalf("disabled after refresh but candidate gate=%q; trace=%+v", a.Gate, h.Last())
		}
	}
}

func TestQADryRunHomeSessionDoesNotPanic(t *testing.T) {
	a := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour})
	defer a.Stop()
	m := NewManager(nil, a, nil)
	m.SetConfig(&cfg.Config{Home: cfg.HomeConfig{Enabled: true}})
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("Home dry run with session_id panicked: %v", p)
		}
	}()
	trace, err := m.DryRunRoute(context.Background(), routing.DryRunRequest{Model: "gpt-5", SessionID: "qa-session"})
	if err != nil {
		t.Fatal(err)
	}
	if trace.Mode != routing.ModeDryRun || trace.DecidedBy != routing.DecidedByHome || trace.Pick != -1 || trace.Candidates == nil {
		t.Fatalf("unexpected Home dry-run trace: %+v", trace)
	}
}

func TestQATraceCaptureKeepsLCPMetadataWithNoRules(t *testing.T) {
	for _, traced := range []bool{false, true} {
		t.Run(fmt.Sprint(traced), func(t *testing.T) {
			const provider = "qa-lcp"
			const model = "qa-lcp-model"
			a := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour})
			defer a.Stop()
			m := NewManager(nil, a, nil)
			var observed map[string]any
			m.RegisterExecutor(scriptedFailoverExecutor{provider: provider, execute: func(_ context.Context, _ *Auth, _ ex.Request, o ex.Options) (ex.Response, error) {
				observed = o.Metadata
				return ex.Response{Payload: []byte("ok")}, nil
			}})
			m.Register(WithSkipPersist(context.Background()), &Auth{ID: "qa-lcp-a", Provider: provider, Status: StatusActive})
			registry.GetGlobalRegistry().RegisterClient("qa-lcp-a", provider, []*registry.ModelInfo{{ID: model}})
			defer registry.GetGlobalRegistry().UnregisterClient("qa-lcp-a")
			ctx := context.Background()
			if traced {
				ctx, _ = routing.WithTraceHolder(ctx)
			}
			opts := ex.Options{SourceFormat: trans.FormatOpenAI, OriginalRequest: []byte(`{"messages":[{"role":"user","content":"hello"}]}`), Metadata: map[string]any{ex.CallerScopeMetadataKey: "qa-caller"}}
			_, err := m.Execute(ctx, []string{provider}, ex.Request{Model: model}, opts)
			if err != nil {
				t.Fatal(err)
			}
			if observed[ex.LCPAffinitySessionIDMetadataKey] == nil || observed[ex.LCPAccessGenerationMetadataKey] == nil {
				t.Fatalf("traced=%v drops LCP metadata: LCP id=%v generation=%v canonical=%v", traced, observed[ex.LCPAffinitySessionIDMetadataKey], observed[ex.LCPAccessGenerationMetadataKey], observed[ex.CanonicalSessionIDMetadataKey])
			}
		})
	}
}

func TestQATraceCaptureKeepsCompactionClassification(t *testing.T) {
	for _, traced := range []bool{false, true} {
		t.Run(fmt.Sprint(traced), func(t *testing.T) {
			const provider = "qa-compact"
			const model = "qa-compact-model"
			a := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour})
			defer a.Stop()
			m := NewManager(nil, a, nil)
			var observed map[string]any
			m.RegisterExecutor(scriptedFailoverExecutor{provider: provider, execute: func(_ context.Context, _ *Auth, _ ex.Request, o ex.Options) (ex.Response, error) {
				observed = o.Metadata
				return ex.Response{Payload: []byte("ok")}, nil
			}})
			m.Register(WithSkipPersist(context.Background()), &Auth{ID: "qa-compact-a", Provider: provider, Status: StatusActive})
			registry.GetGlobalRegistry().RegisterClient("qa-compact-a", provider, []*registry.ModelInfo{{ID: model}})
			defer registry.GetGlobalRegistry().UnregisterClient("qa-compact-a")
			payloads := []string{`{"contents":[{"role":"user","parts":[{"text":"step 1"}]},{"role":"model","parts":[{"text":"ack 1"}]},{"role":"user","parts":[{"text":"step 2"}]},{"role":"model","parts":[{"text":"ack 2"}]},{"role":"user","parts":[{"text":"step 3"}]}]}`, `{"contents":[{"role":"user","parts":[{"text":"<summary>Steps 1 and 2 completed</summary>"}]},{"role":"model","parts":[{"text":"ack 2"}]},{"role":"user","parts":[{"text":"step 3"}]},{"role":"user","parts":[{"text":"step 4"}]}]}`}
			for _, payload := range payloads {
				ctx := context.Background()
				if traced {
					ctx, _ = routing.WithTraceHolder(ctx)
				}
				o := ex.Options{SourceFormat: trans.FormatGemini, OriginalRequest: []byte(payload), Metadata: map[string]any{ex.CallerScopeMetadataKey: "qa-compact-caller"}}
				if _, err := m.Execute(ctx, []string{provider}, ex.Request{Model: model}, o); err != nil {
					t.Fatal(err)
				}
			}
			if observed[ex.IsCompactionMetadataKey] != true || observed[ex.NodeKindMetadataKey] != "compaction" {
				t.Fatalf("traced=%v: compaction=%v node_kind=%v", traced, observed[ex.IsCompactionMetadataKey], observed[ex.NodeKindMetadataKey])
			}
		})
	}
}

func TestRoutingMissingTargetSerializesEmptyArray(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	m.RegisterExecutor(schedulerTestExecutor{provider: "qa-null"})
	m.Register(WithSkipPersist(context.Background()), &Auth{ID: "present", Provider: "qa-null", Status: StatusActive})
	c, _ := cfg.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [absent]\n"))
	m.SetConfig(c)
	ctx, h := routing.WithTraceHolder(context.Background())
	_, _, err := m.pickNext(ctx, "qa-null", "", ex.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(h.Last())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"targets":[]`) {
		t.Fatalf("missing-target trace must serialize an empty array: %s", data)
	}
}

func TestQASchedulerTracePickIsInTier(t *testing.T) {
	const provider = "codex"
	const model = "qa-ws-model"
	m := NewManager(nil, &FillFirstSelector{}, nil)
	m.RegisterExecutor(schedulerTestExecutor{provider: provider})
	for _, a := range []*Auth{{ID: "qa-http", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "10"}}, {ID: "qa-ws", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "0", "websockets": "true"}}} {
		m.Register(WithSkipPersist(context.Background()), a)
		registry.GetGlobalRegistry().RegisterClient(a.ID, provider, []*registry.ModelInfo{{ID: model}})
		id := a.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	ctx, h := routing.WithTraceHolder(ex.WithDownstreamWebsocket(context.Background()))
	got, _, err := m.pickNext(ctx, provider, model, ex.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := h.Last()
	if got.ID != "qa-ws" || tr.Tier == nil || tr.Tier.Priority != 0 || len(tr.Tier.Pool) != 1 || tr.Tier.Pool[0] != tr.Pick {
		t.Fatalf("selected %s (index %d), expected only the websocket tier; got %+v", got.ID, tr.Pick, tr.Tier)
	}
}

func TestQANoHolderCaptureAllocations(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	ctx := context.Background()
	providers := []string{"qa-provider"}
	opts := ex.Options{}
	allocations := testing.AllocsPerRun(1000, func() {
		next, capture := m.beginRoutingCapture(ctx, providers, "model", opts, nil)
		if next != ctx || capture != nil {
			panic("unexpected trace capture")
		}
	})
	t.Logf("beginRoutingCapture with no holder: %v allocs/op", allocations)
	if allocations != 0 {
		t.Fatalf("allocations=%v", allocations)
	}
}

func TestQAGSRDoesNotNarrowPluginHandledPick(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint(mixed), func(t *testing.T) {
			const provider = "qa-plugin-gsr"
			m := NewManager(nil, &GreedySoonestRecoverySelector{}, nil)
			m.RegisterExecutor(schedulerTestExecutor{provider: provider})
			for _, a := range []*Auth{{ID: "outside", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "10"}}, {ID: "target", Provider: provider, Status: StatusActive}} {
				m.Register(WithSkipPersist(context.Background()), a)
			}
			plugin := &fakePluginScheduler{handled: true, resp: pluginapi.SchedulerPickResponse{Handled: true, AuthID: "outside"}}
			m.SetPluginScheduler(plugin)
			c, _ := cfg.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [target]\n"))
			m.SetConfig(c)
			var got *Auth
			var err error
			if mixed {
				got, _, _, err = m.pickNextMixed(context.Background(), []string{provider}, "", ex.Options{}, nil)
			} else {
				got, _, err = m.pickNext(context.Background(), provider, "", ex.Options{}, nil)
			}
			if err != nil || got == nil || got.ID != "outside" {
				t.Fatalf("plugin requested outside (eligible), got=%v err=%v; offered candidates=%+v", got, err, plugin.requests[0].Candidates)
			}
		})
	}
}

func TestQAProgrammaticInvalidRulesRetainWholeConfig(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	valid, _ := cfg.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [previous]\n"))
	m.SetConfig(valid)
	invalid := valid.CloneForRuntime()
	invalid.Routing.Rules[0].RouteTo = nil
	invalid.Home.Enabled = true
	m.SetConfig(invalid)
	current, _ := m.runtimeConfig.Load().(*cfg.Config)
	if current.Home.Enabled || len(current.Routing.Rules[0].RouteTo) == 0 {
		t.Fatalf("invalid new runtime config was published while old compiled rules remain: old decision=%+v", m.compiledRoutingRules().Decide(routing.RequestAttrs{}, []string{"previous"}))
	}
}

func TestQAPluginBuiltinDelegationKeepsEligibleAffinity(t *testing.T) {
	const provider = "qa-delegate"
	a := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour})
	defer a.Stop()
	m := NewManager(nil, a, nil)
	m.RegisterExecutor(schedulerTestExecutor{provider: provider})
	for _, id := range []string{"bound", "target"} {
		m.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: provider, Status: StatusActive})
	}
	opts := ex.Options{Metadata: map[string]any{ex.DerivedSessionIDMetadataKey: "qa-session"}}
	_, err := a.Pick(context.Background(), provider, "", opts, []*Auth{{ID: "bound", Provider: provider, Status: StatusActive}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := cfg.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [target]\n"))
	m.SetConfig(c)
	m.SetPluginScheduler(&fakePluginScheduler{handled: true, resp: pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin}})
	ctx, holder := routing.WithTraceHolder(context.Background())
	got, _, err := m.pickNext(ctx, provider, "", opts, nil)
	if err != nil || got == nil || got.ID != "bound" {
		t.Fatalf("eligible bound credential should outrank local delegated rule, got=%v err=%v", got, err)
	}
	if trace := holder.Last(); trace == nil || trace.DecidedBy != routing.DecidedByAffinity {
		t.Fatalf("trace=%+v", trace)
	}
}

func TestRoutingLegacyWRRWithoutRulesPreservesHighestTierBehavior(t *testing.T) {
	m := NewManager(nil, &WeightedRoundRobinSelector{}, nil)
	m.RegisterExecutor(schedulerTestExecutor{provider: "qa-wrr-legacy"})
	for _, a := range []*Auth{
		{ID: "zero-high", Provider: "qa-wrr-legacy", Status: StatusActive, Attributes: map[string]string{"priority": "10", "weight": "0"}},
		{ID: "positive-low", Provider: "qa-wrr-legacy", Status: StatusActive, Attributes: map[string]string{"priority": "0", "weight": "1"}},
	} {
		if _, err := m.Register(WithSkipPersist(context.Background()), a); err != nil {
			t.Fatal(err)
		}
	}
	selected, _, err := m.pickNextLegacy(context.Background(), "qa-wrr-legacy", "", ex.Options{}, nil)
	if selected != nil || err == nil {
		t.Fatalf("selected=%v err=%v; legacy highest tier has no positive weight", selected, err)
	}
}

func TestRoutingTraceHolderPreservesExecutionMetadata(t *testing.T) {
	const provider = "qa-compare"
	const model = "qa-compare-model"
	execute := func(traced bool) map[string]any {
		t.Helper()
		affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour})
		defer affinity.Stop()
		m := NewManager(nil, affinity, nil)
		var observed map[string]any
		m.RegisterExecutor(scriptedFailoverExecutor{provider: provider, execute: func(_ context.Context, _ *Auth, _ ex.Request, opts ex.Options) (ex.Response, error) {
			observed = maps.Clone(opts.Metadata)
			return ex.Response{Payload: []byte("ok")}, nil
		}})
		if _, err := m.Register(WithSkipPersist(context.Background()), &Auth{ID: "qa-compare-a", Provider: provider, Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient("qa-compare-a", provider, []*registry.ModelInfo{{ID: model}})
		defer registry.GetGlobalRegistry().UnregisterClient("qa-compare-a")
		ctx := context.Background()
		if traced {
			ctx, _ = routing.WithTraceHolder(ctx)
		}
		opts := ex.Options{SourceFormat: trans.FormatOpenAI, OriginalRequest: []byte(`{"messages":[{"role":"user","content":"hello"}]}`), Metadata: map[string]any{ex.CallerScopeMetadataKey: "compare-caller"}}
		if _, err := m.Execute(ctx, []string{provider}, ex.Request{Model: model}, opts); err != nil {
			t.Fatal(err)
		}
		return observed
	}
	plain, traced := execute(false), execute(true)
	if plain[ex.SelectedAuthCallbackMetadataKey] == nil || traced[ex.SelectedAuthCallbackMetadataKey] == nil {
		t.Fatal("missing selection callback")
	}
	delete(plain, ex.SelectedAuthCallbackMetadataKey)
	delete(traced, ex.SelectedAuthCallbackMetadataKey)
	if !reflect.DeepEqual(plain, traced) {
		t.Fatalf("execution metadata differs:\nwithout holder: %#v\nwith holder: %#v", plain, traced)
	}
}

func TestRoutingTraceRequiredSlicesSerializeAsArrays(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	ctx, h := routing.WithTraceHolder(context.Background())
	_, _, _ = m.pickNext(ctx, "missing-provider", "", ex.Options{}, nil)
	data, err := json.Marshal(h.Last())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"candidates":[]`) {
		t.Fatalf("missing empty candidates array: %s", data)
	}
	trace := &routing.Trace{Candidates: []routing.Candidate{}, Rules: &routing.RulesStep{Matched: []routing.RuleTrace{{Targets: []int{}}}}, Tier: &routing.TierStep{Pool: []int{}}}
	data, err = json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"candidates":[]`, `"targets":[]`, `"pool":[]`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("missing %s: %s", want, data)
		}
	}
}

func TestRoutingPluginHandledGSRDoesNotRefreshRulePool(t *testing.T) {
	const provider = "qa-plugin-refresh"
	m := NewManager(nil, &GreedySoonestRecoverySelector{}, nil)
	m.RegisterExecutor(schedulerTestExecutor{provider: provider})
	for _, id := range []string{"plugin-outside", "plugin-target-a", "plugin-target-b"} {
		if _, err := m.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: provider, Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
	}
	plugin := &fakePluginScheduler{handled: true, resp: pluginapi.SchedulerPickResponse{Handled: true, AuthID: "plugin-outside"}}
	m.SetPluginScheduler(plugin)
	c, err := cfg.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [plugin-target-a, plugin-target-b]\n"))
	if err != nil {
		t.Fatal(err)
	}
	m.SetConfig(c)
	var refreshes atomic.Int32
	m.SetQuotaRecoveryRefresher(func(_ context.Context, _ *Auth, _ string) error { refreshes.Add(1); return nil })
	selected, _, err := m.pickNext(context.Background(), provider, "", ex.Options{}, nil)
	if err != nil || selected == nil || selected.ID != "plugin-outside" || refreshes.Load() != 0 {
		t.Fatalf("selection=%v err=%v refreshes=%d", selected, err, refreshes.Load())
	}
}

func TestRoutingGSRTraceNoEligibleAfterRefresh(t *testing.T) {
	const provider = "qa-empty-refresh"
	m := NewManager(nil, &GreedySoonestRecoverySelector{}, nil)
	m.RegisterExecutor(schedulerTestExecutor{provider: provider})
	for _, id := range []string{"refresh-a", "refresh-b"} {
		if _, err := m.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: provider, Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := cfg.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [refresh-a, refresh-b]\n"))
	if err != nil {
		t.Fatal(err)
	}
	m.SetConfig(c)
	m.SetQuotaRecoveryRefresher(func(ctx context.Context, a *Auth, _ string) error {
		a.Disabled = true
		_, errUpdate := m.Update(WithSkipPersist(ctx), a)
		return errUpdate
	})
	ctx, h := routing.WithTraceHolder(context.Background())
	selected, _, err := m.pickNext(ctx, provider, "", ex.Options{}, nil)
	if err == nil || selected != nil {
		t.Fatalf("selected=%v err=%v", selected, err)
	}
	trace := h.Last()
	if trace == nil || trace.Error != "no_eligible" {
		t.Fatalf("trace=%+v", trace)
	}
	for _, c := range trace.Candidates {
		if c.Gate != routing.GateDisabled {
			t.Fatalf("candidate=%+v", c)
		}
	}
}
