package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
	session "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

func TestDryRunFindsRealHeaderBindingAndExplainsOverride(t *testing.T) {
	const provider = "e2e-routing"
	const model = "e2e-routing-model"
	const apiKey = "test-client-key"
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour})
	defer affinity.Stop()
	manager := NewManager(nil, affinity, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
	for _, a := range []*Auth{{ID: "e2e-bound", Provider: provider, Status: StatusActive}, {ID: "e2e-target", Provider: provider, Status: StatusActive}} {
		if _, err := manager.Register(WithSkipPersist(context.Background()), a); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(a.ID, provider, []*registry.ModelInfo{{ID: model}})
		id := a.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	opts := ex.Options{Headers: http.Header{"X-Session-Id": []string{"qa-sess-f-1"}}, Metadata: map[string]any{ex.CallerScopeMetadataKey: session.CallerScope(apiKey), ex.ClientKeyIDMetadataKey: internalconfig.APIKeyID(apiKey)}}
	selected, _, err := manager.pickNext(context.Background(), provider, model, opts, nil)
	if err != nil || selected == nil || selected.ID != "e2e-bound" {
		t.Fatalf("initial=%v err=%v", selected, err)
	}
	cfg, err := internalconfig.ParseConfigBytes([]byte("api-keys: [test-client-key]\nrouting:\n  rules:\n    - when: { header: { name: X-Session-ID, is: qa-sess-f-1 } }\n      route-to: [e2e-target]\n    - route-to: [missing-target]\n"))
	if err != nil {
		t.Fatal(err)
	}
	manager.SetConfig(cfg)
	ctx, holder := routing.WithTraceHolder(context.Background())
	selected, _, err = manager.pickNext(ctx, provider, model, opts, nil)
	if err != nil || selected == nil || selected.ID != "e2e-bound" {
		t.Fatalf("bound=%v err=%v", selected, err)
	}
	assertOverride := func(t *testing.T, trace *routing.Trace) {
		t.Helper()
		if trace == nil || trace.DecidedBy != routing.DecidedByAffinity || trace.Affinity.State != routing.AffinityKept || trace.Rules == nil || trace.Rules.Applied != -1 || len(trace.Rules.Matched) != 2 || trace.Rules.Matched[0].Status != routing.RuleOverridden || trace.Rules.Matched[1].Status != routing.RuleFellThrough {
			t.Fatalf("trace=%+v", trace)
		}
	}
	cacheKey := provider + "::header:qa-sess-f-1::" + canonicalModelKey(model)
	affinity.cache.mu.RLock()
	before := affinity.cache.entries[cacheKey]
	affinity.cache.mu.RUnlock()
	for _, test := range []struct {
		name string
		req  routing.DryRunRequest
	}{
		{"raw id", routing.DryRunRequest{Model: model, KeyID: internalconfig.APIKeyID(apiKey), SessionID: "qa-sess-f-1"}},
		{"explicit header", routing.DryRunRequest{Model: model, KeyID: internalconfig.APIKeyID(apiKey), SessionID: "different", Headers: http.Header{"X-Session-Id": []string{"qa-sess-f-1"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			trace, errDry := manager.DryRunRoute(context.Background(), test.req)
			if errDry != nil || trace.Pick < 0 || trace.Candidates[trace.Pick].ID != "e2e-bound" {
				t.Fatalf("dry trace=%+v err=%v", trace, errDry)
			}
			assertOverride(t, trace)
		})
	}
	assertOverride(t, holder.Last())
	affinity.cache.mu.RLock()
	after := affinity.cache.entries[cacheKey]
	affinity.cache.mu.RUnlock()
	if before.authID != after.authID || !before.expiresAt.Equal(after.expiresAt) {
		t.Fatalf("dry run changed binding: %v -> %v", before, after)
	}
}

func TestAffinityDoesNotEvaluateRulesWithoutTrace(t *testing.T) {
	a := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour})
	defer a.Stop()
	opts := ex.Options{Headers: http.Header{"X-Session-Id": []string{"no-trace"}}, Metadata: map[string]any{}}
	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	if _, err := a.Pick(context.Background(), "provider", "model", opts, auths); err != nil {
		t.Fatal(err)
	}
	calls := 0
	ctx := context.WithValue(context.Background(), routingPoolKey{}, &routingPool{decide: func(_ routing.RequestAttrs, _ []string) routing.Decision {
		calls++
		return routing.Decision{Allowed: []string{"b"}}
	}})
	picked, err := a.Pick(ctx, "provider", "model", opts, auths)
	if err != nil || picked.ID != "a" || calls != 0 {
		t.Fatalf("pick=%v calls=%d err=%v", picked, calls, err)
	}
}

func TestDryRunUnpredictedStrategiesAreStillDecidedByStrategy(t *testing.T) {
	for _, test := range []struct {
		name     string
		selector Selector
	}{{"weighted", &WeightedRoundRobinSelector{}}, {"gsr", &GreedySoonestRecoverySelector{}}} {
		t.Run(test.name, func(t *testing.T) {
			const provider = "e2e-unpredicted"
			const model = "e2e-unpredicted-model"
			manager := NewManager(nil, test.selector, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
			if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: "eligible", Provider: provider, Status: StatusActive}); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient("eligible", provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("eligible") })
			trace, err := manager.DryRunRoute(context.Background(), routing.DryRunRequest{Model: model})
			if err != nil || trace.DecidedBy != routing.DecidedByStrategy || trace.Pick != -1 || trace.PickPredicted || trace.Tier == nil || len(trace.Tier.Pool) != 1 {
				t.Fatalf("trace=%+v err=%v", trace, err)
			}
			auth, ok := manager.GetByID("eligible")
			if !ok {
				t.Fatal("registered credential missing")
			}
			auth.Disabled = true
			if _, errUpdate := manager.Update(WithSkipPersist(context.Background()), auth); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			trace, err = manager.DryRunRoute(context.Background(), routing.DryRunRequest{Model: model})
			if err != nil || trace.DecidedBy != routing.DecidedByNone || trace.Error != "no_eligible" || trace.Pick != -1 {
				t.Fatalf("empty trace=%+v err=%v", trace, err)
			}
		})
	}
}

func TestDryRunMatchesLiveAffinityOnFailover(t *testing.T) {
	for _, test := range []struct {
		name   string
		rebind string
		status int
		want   string
	}{
		{"transient quota policy", SessionAffinityRebindOnQuota, http.StatusServiceUnavailable, routing.AffinityHeld},
		{"quota failure", SessionAffinityRebindOnQuota, http.StatusTooManyRequests, routing.AffinityNew},
		{"transient rebind any", SessionAffinityRebindOnAny, http.StatusServiceUnavailable, routing.AffinityNew},
	} {
		t.Run(test.name, func(t *testing.T) {
			const provider = "dry-held-test"
			const model = "dry-held-model"
			affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour, RebindOn: test.rebind})
			defer affinity.Stop()
			manager := NewManager(nil, affinity, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
			for _, a := range []*Auth{{ID: "held-a", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "1"}}, {ID: "held-b", Provider: provider, Status: StatusActive}} {
				if _, err := manager.Register(WithSkipPersist(context.Background()), a); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(a.ID, provider, []*registry.ModelInfo{{ID: model}})
				id := a.ID
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}
			opts := ex.Options{Headers: http.Header{"X-Session-Id": []string{"held-session"}}, Metadata: map[string]any{}}
			initial, _, err := manager.pickNext(context.Background(), provider, model, opts, nil)
			if err != nil || initial == nil || initial.ID != "held-a" {
				t.Fatalf("initial=%v err=%v", initial, err)
			}
			manager.MarkResult(context.Background(), Result{AuthID: "held-a", Provider: provider, Model: model, Success: false, Error: &Error{HTTPStatus: test.status, Message: "upstream failure"}, Options: opts})
			key := provider + "::header:held-session::" + canonicalModelKey(model)
			affinity.cache.mu.RLock()
			before := affinity.cache.entries[key]
			beforeCount := len(affinity.cache.entries)
			affinity.cache.mu.RUnlock()
			beforeVersion := manager.syncedVersion.Load()
			dry, err := manager.DryRunRoute(context.Background(), routing.DryRunRequest{Model: model, SessionID: "held-session"})
			if err != nil {
				t.Fatal(err)
			}
			if dry.Pick < 0 || dry.Candidates[dry.Pick].ID != "held-b" || dry.Affinity.State != test.want {
				t.Fatalf("dry trace=%+v affinity=%+v", dry, dry.Affinity)
			}
			affinity.cache.mu.RLock()
			after := affinity.cache.entries[key]
			afterCount := len(affinity.cache.entries)
			affinity.cache.mu.RUnlock()
			if before.authID != after.authID || !before.expiresAt.Equal(after.expiresAt) || beforeCount != afterCount || beforeVersion != manager.syncedVersion.Load() {
				t.Fatal("dry run changed affinity or scheduler state")
			}
			ctx, holder := routing.WithTraceHolder(context.Background())
			live, _, err := manager.pickNext(ctx, provider, model, opts, nil)
			if err != nil || live == nil || live.ID != "held-b" {
				t.Fatalf("live=%v err=%v", live, err)
			}
			actual := holder.Last()
			if actual == nil || actual.Affinity.State != dry.Affinity.State {
				t.Fatalf("dry=%+v live=%+v", dry.Affinity, actual)
			}
			if test.want == routing.AffinityHeld && (actual.Affinity.Bound < 0 || dry.Affinity.Bound < 0 || actual.Candidates[actual.Affinity.Bound].ID != dry.Candidates[dry.Affinity.Bound].ID) {
				t.Fatalf("bound differs: dry=%+v live=%+v", dry.Affinity, actual)
			}
			if test.want == routing.AffinityNew && (actual.Affinity.Bound != -1 || dry.Affinity.Bound != -1) {
				t.Fatalf("unexpected old binding: dry=%+v live=%+v", dry.Affinity, actual)
			}
			affinity.cache.mu.RLock()
			final := affinity.cache.entries[key]
			affinity.cache.mu.RUnlock()
			wantBinding := "held-b"
			if test.want == routing.AffinityHeld {
				wantBinding = "held-a"
			}
			if final.authID != wantBinding {
				t.Fatalf("live binding=%q want=%q", final.authID, wantBinding)
			}
		})
	}
}

func TestAffinityRuleTraceAgreementAndConflict(t *testing.T) {
	for _, test := range []struct {
		name, targets string
		agrees        bool
	}{{"agrees", "[trace-a, trace-b]", true}, {"conflicts", "[trace-b]", false}} {
		t.Run(test.name, func(t *testing.T) {
			const provider = "affinity-rule-test"
			const model = "affinity-rule-model"
			affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour})
			defer affinity.Stop()
			manager := NewManager(nil, affinity, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
			for _, id := range []string{"trace-a", "trace-b"} {
				if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: provider, Status: StatusActive}); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
				id := id
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}
			opts := ex.Options{Headers: http.Header{"X-Session-Id": []string{"agreement-session"}}, Metadata: map[string]any{}}
			selected, _, err := manager.pickNext(context.Background(), provider, model, opts, nil)
			if err != nil || selected == nil || selected.ID != "trace-a" {
				t.Fatalf("initial=%v err=%v", selected, err)
			}
			cfg, err := internalconfig.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [trace-b]\n    - route-to: " + test.targets + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			manager.SetConfig(cfg)
			ctx, holder := routing.WithTraceHolder(context.Background())
			selected, _, err = manager.pickNext(ctx, provider, model, opts, nil)
			if err != nil || selected == nil || selected.ID != "trace-a" {
				t.Fatalf("selected=%v err=%v", selected, err)
			}
			dry, err := manager.DryRunRoute(context.Background(), routing.DryRunRequest{Model: model, SessionID: "agreement-session"})
			if err != nil {
				t.Fatal(err)
			}
			for label, trace := range map[string]*routing.Trace{"live": holder.Last(), "dry": dry} {
				if trace == nil || trace.DecidedBy != routing.DecidedByAffinity || trace.Affinity.State != routing.AffinityKept || trace.Rules == nil || len(trace.Rules.Matched) != 2 || trace.Rules.Matched[0].Status != routing.RuleOverridden {
					t.Fatalf("%s trace=%+v", label, trace)
				}
				if test.agrees {
					if trace.Rules.Applied != 1 || trace.Rules.Matched[1].Status != routing.RuleApplied || len(trace.Rules.Pool) != 2 {
						t.Fatalf("%s rule trace=%+v", label, trace.Rules)
					}
					ids := map[string]bool{}
					for _, index := range trace.Rules.Pool {
						ids[trace.Candidates[index].ID] = true
					}
					if !ids["trace-a"] || !ids["trace-b"] {
						t.Fatalf("%s pool=%v", label, ids)
					}
				} else if trace.Rules.Applied != -1 || trace.Rules.Matched[1].Status != routing.RuleOverridden || len(trace.Rules.Pool) != 0 {
					t.Fatalf("%s rule trace=%+v", label, trace.Rules)
				}
			}
		})
	}
}
