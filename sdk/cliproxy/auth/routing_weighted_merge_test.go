package auth

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Include unexported accounting fields, which JSON snapshots silently omit.
func snapshotWeightedSelectorForTest(s *WeightedRoundRobinSelector) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	exact := make(map[[32]byte][]weightedPoolState, len(s.states))
	copyState := func(state *weightedPoolState) weightedPoolState {
		copy := *state
		copy.members = slices.Clone(state.members)
		copy.lastActiveIDs = slices.Clone(state.lastActiveIDs)
		copy.next = nil
		return copy
	}
	for key, state := range s.states {
		for ; state != nil; state = state.next {
			exact[key] = append(exact[key], copyState(state))
		}
	}
	large := make(map[string]weightedPoolState, len(s.largeStates))
	for key, state := range s.largeStates {
		large[key] = copyState(state)
	}
	return []any{exact, large, slices.Clone(s.largeOrder), s.stateCount, s.maxKeys}
}

func TestRoutingWeightedSimulationCopiesReservations(t *testing.T) {
	for _, scenario := range []string{"pending", "subset", "dynamic-subset", "weight-change", "epoch-change", "digest-collision", "saturated-binding"} {
		t.Run(scenario, func(t *testing.T) {
			s := &WeightedRoundRobinSelector{}
			pool := []*Auth{{ID: "a", Attributes: map[string]string{"weight": "3"}}, {ID: "b"}}
			if scenario == "dynamic-subset" {
				for i := range maxExactWeightedPoolMembers {
					pool = append(pool, &Auth{ID: fmt.Sprintf("c-%04d", i)})
				}
			}
			const route = "mixed:public-alias"
			for range 4 {
				_, reservation := s.reserveScoped(route, 0, "ws", pool, "b")
				reservation.finish(true)
			}
			if scenario == "saturated-binding" {
				for range 200 {
					_, reservation := s.reserveScoped(route, 0, "ws", pool, "b")
					reservation.finish(true)
				}
			}
			_, pending := s.reserveScoped(route, 0, "ws", pool, "b")
			defer pending.finish(false)
			switch scenario {
			case "subset", "dynamic-subset":
				subset := pool[1:]
				_, reservation := s.reserveScoped(route, 0, "ws", subset, "")
				reservation.finish(true)
			case "weight-change":
				pool[0].Attributes["weight"] = "2"
			case "epoch-change":
				pool[0].RegistrationEpoch = 1
				_, reservation := s.reserveScoped(route, 0, "ws", pool, "")
				reservation.finish(true)
				pool[0].RegistrationEpoch = 2
			case "digest-collision":
				key := weightedPoolStateKey(route, authWeightVector(pool))
				s.states[key] = &weightedPoolState{route: "unrelated", members: []weightedPoolMember{{id: "other", weight: 100}}, next: s.states[key]}
				s.stateCount++
			}
			before := snapshotWeightedSelectorForTest(s)
			_, capture := telemetryCapture()
			step, sim := s.weightedStrategySimulation(capture, route, 0, "ws", pool)
			if scenario == "pending" && step.Weights[0].Credit != 15 {
				t.Fatalf("pending credit omitted: %+v", step.Weights)
			}
			predicted := make([]string, 0, 10)
			for range 10 {
				predicted = append(predicted, sim.next().ID)
			}
			if !reflect.DeepEqual(before, snapshotWeightedSelectorForTest(s)) {
				t.Fatal("simulation changed live members, credits, reservations, generations, or pool membership")
			}
			for i, expected := range predicted {
				picked, reservation := s.reserveScoped(route, 0, "ws", pool, "")
				reservation.finish(true)
				if picked.ID != expected {
					t.Fatalf("prediction %d = %s, live = %s", i, expected, picked.ID)
				}
			}
		})
	}
}

func TestRoutingWeightedDryRunAfterAffinityTraffic(t *testing.T) {
	for _, withRules := range []bool{false, true} {
		t.Run(fmt.Sprintf("rules=%t", withRules), func(t *testing.T) {
			weighted := &WeightedRoundRobinSelector{}
			affinity := NewSessionAffinitySelector(weighted)
			defer affinity.Stop()
			manager := NewManager(nil, affinity, nil)
			const provider, model = "merge-wrr", "merge-wrr-model"
			manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
			for _, a := range []*Auth{{ID: "a", Provider: provider, Attributes: map[string]string{"weight": "3"}}, {ID: "b", Provider: provider}} {
				if _, err := manager.Register(WithSkipPersist(t.Context()), a); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(a.ID, provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
			}
			opts := ex.Options{Headers: http.Header{"X-Session-Id": {"sticky"}}}
			key := provider + "::" + CanonicalSessionID(opts.Headers, nil, nil) + "::" + model
			affinity.cache.SetAliases("b", key)
			if withRules {
				cfg, err := internalconfig.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [a]\n"))
				if err != nil {
					t.Fatal(err)
				}
				manager.SetConfig(cfg)
			}
			for range 4 {
				ctx, holder := routing.WithTraceHolder(t.Context())
				picked, _, _, err := manager.pickNextMixed(ctx, []string{provider}, model, opts, nil)
				if err != nil || picked.ID != "b" {
					t.Fatalf("sticky selection = %v, %v", picked, err)
				}
				trace := holder.Last()
				if trace.Candidates[trace.StrategyStep.Wanted].ID != "a" || trace.Candidates[trace.Pick].ID != "b" {
					t.Fatalf("strategy wanted was overwritten by affinity: %+v", trace)
				}
			}
			before := snapshotWeightedSelectorForTest(weighted)
			dry, err := manager.DryRunRoute(context.Background(), routing.DryRunRequest{Model: model})
			if err != nil || dry.StrategyStep == nil || len(dry.StrategyStep.Next) != 10 {
				t.Fatalf("dry run = %+v, %v", dry, err)
			}
			if !reflect.DeepEqual(before, snapshotWeightedSelectorForTest(weighted)) {
				t.Fatal("dry run changed live WRR accounting")
			}
			for i, index := range dry.StrategyStep.Next {
				picked, _, _, err := manager.pickNextMixed(t.Context(), []string{provider}, model, ex.Options{}, nil)
				if err != nil || picked.ID != dry.Candidates[index].ID {
					t.Fatalf("pick %d = %v, %v, predicted %s", i, picked, err, dry.Candidates[index].ID)
				}
			}
		})
	}
}

func TestRoutingDryRunNeverRebindsUnavailableAffinity(t *testing.T) {
	for _, weighted := range []bool{false, true} {
		t.Run(fmt.Sprintf("weighted=%t", weighted), func(t *testing.T) {
			var selector Selector = &RoundRobinSelector{}
			if weighted {
				selector = &WeightedRoundRobinSelector{}
			}
			affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: selector, RebindOn: SessionAffinityRebindOnNever})
			defer affinity.Stop()
			manager := NewManager(nil, affinity, nil)
			const provider, model = "merge-never", "merge-never-model"
			manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
			for _, id := range []string{"a", "b"} {
				if _, err := manager.Register(WithSkipPersist(t.Context()), &Auth{ID: id, Provider: provider}); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}
			opts := ex.Options{Headers: http.Header{"X-Session-Id": {"never"}}}
			if _, _, err := manager.pickNext(t.Context(), provider, model, opts, nil); err != nil {
				t.Fatal(err)
			}
			manager.MarkResult(t.Context(), Result{AuthID: "a", Provider: provider, Model: model, Error: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "temporarily unavailable"}})
			key := provider + "::" + CanonicalSessionID(opts.Headers, nil, nil) + "::" + model
			affinity.cache.mu.RLock()
			before := affinity.cache.entries[key]
			affinity.cache.mu.RUnlock()
			dry, err := manager.DryRunRoute(t.Context(), routing.DryRunRequest{Model: model, Headers: opts.Headers})
			if err != nil || dry.Pick != -1 || dry.PickPredicted || dry.Error != "no_eligible" {
				t.Fatalf("never-mode dry run predicted a failover: %+v, %v", dry, err)
			}
			affinity.cache.mu.RLock()
			after := affinity.cache.entries[key]
			affinity.cache.mu.RUnlock()
			if !reflect.DeepEqual(before, after) {
				t.Fatal("dry run touched the binding or its provenance")
			}
			if picked, _, err := manager.pickNext(t.Context(), provider, model, opts, nil); picked != nil || !isSessionAffinityRetryError(err) {
				t.Fatalf("live never-mode selection = %v, %v", picked, err)
			}
		})
	}
}

type routingMergeExecutor struct{ schedulerTestExecutor }

func (routingMergeExecutor) Execute(_ context.Context, auth *Auth, _ ex.Request, _ ex.Options) (ex.Response, error) {
	return ex.Response{Payload: []byte(auth.ID)}, nil
}

func (routingMergeExecutor) CountTokens(_ context.Context, auth *Auth, _ ex.Request, _ ex.Options) (ex.Response, error) {
	return ex.Response{Payload: []byte(auth.ID)}, nil
}

func (routingMergeExecutor) ExecuteStream(_ context.Context, auth *Auth, _ ex.Request, _ ex.Options) (*ex.StreamResult, error) {
	chunks := make(chan ex.StreamChunk, 1)
	chunks <- ex.StreamChunk{Payload: []byte(auth.ID)}
	close(chunks)
	return &ex.StreamResult{Chunks: chunks}, nil
}

func TestRoutingWeightedDryRunMatchesPublicExecution(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "count"} {
		for _, withRules := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rules=%t", mode, withRules), func(t *testing.T) {
				selector := &WeightedRoundRobinSelector{}
				manager := NewManager(nil, selector, nil)
				const provider, model = "merge-execution", "merge-public-model"
				manager.RegisterExecutor(routingMergeExecutor{schedulerTestExecutor{provider: provider}})
				for _, auth := range []*Auth{{ID: "a", Provider: provider, Attributes: map[string]string{"weight": "3"}}, {ID: "b", Provider: provider}} {
					if _, err := manager.Register(WithSkipPersist(t.Context()), auth); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				}
				if withRules {
					cfg, err := internalconfig.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [a, b]\n"))
					if err != nil {
						t.Fatal(err)
					}
					manager.SetConfig(cfg)
				}
				run := func(opts ex.Options) string {
					t.Helper()
					req := ex.Request{Model: model, Payload: []byte(`{"model":"merge-public-model"}`)}
					if mode == "stream" {
						stream, err := manager.ExecuteStream(t.Context(), []string{provider}, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						var body []byte
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							body = append(body, chunk.Payload...)
						}
						return string(body)
					}
					var response ex.Response
					var err error
					if mode == "count" {
						response, err = manager.ExecuteCount(t.Context(), []string{provider}, req, opts)
					} else {
						response, err = manager.Execute(t.Context(), []string{provider}, req, opts)
					}
					if err != nil {
						t.Fatal(err)
					}
					return string(response.Payload)
				}
				for range 4 {
					if got := run(ex.Options{Metadata: map[string]any{ex.PinnedAuthMetadataKey: "b"}}); got != "b" {
						t.Fatalf("pinned execution = %s", got)
					}
				}
				manager.syncScheduler()
				before := snapshotWeightedSelectorForTest(selector)
				dry, err := manager.DryRunRoute(t.Context(), routing.DryRunRequest{Model: model})
				if err != nil || dry.StrategyStep == nil || len(dry.StrategyStep.Next) != 10 {
					t.Fatalf("dry run = %+v, %v", dry, err)
				}
				if !reflect.DeepEqual(before, snapshotWeightedSelectorForTest(selector)) {
					t.Fatal("dry run changed public-execution credits")
				}
				for i, index := range dry.StrategyStep.Next {
					if got, want := run(ex.Options{}), dry.Candidates[index].ID; got != want {
						t.Fatalf("public execution %d = %s, predicted %s", i, got, want)
					}
				}
			})
		}
	}
}

func TestRoutingWeightedBoundTraceUsesStrategyPool(t *testing.T) {
	for _, gate := range []string{"priority", "transport", "rule"} {
		t.Run(gate, func(t *testing.T) {
			weighted := &WeightedRoundRobinSelector{}
			affinity := NewSessionAffinitySelector(weighted)
			defer affinity.Stop()
			pool := []*Auth{{ID: "a"}, {ID: "b"}}
			ctx, capture := telemetryCapture()
			if gate == "priority" {
				pool[0].Attributes = map[string]string{"priority": "10"}
			}
			if gate == "transport" {
				pool[0].Attributes = map[string]string{"websockets": "true"}
				ctx = ex.WithDownstreamWebsocket(ctx)
			}
			if gate == "rule" {
				ctx = context.WithValue(ctx, routingPoolKey{}, &routingPool{decide: func(routing.RequestAttrs, []string) routing.Decision {
					return routing.Decision{Allowed: []string{"a"}}
				}})
			}
			opts := ex.Options{Headers: http.Header{"X-Session-Id": {"bound-trace"}}}
			affinity.cache.Set("codex::"+CanonicalSessionID(opts.Headers, nil, nil)+"::model", "b")
			picked, err := affinity.Pick(ctx, "codex", "model", opts, pool)
			if err != nil || picked.ID != "b" {
				t.Fatalf("affinity selection = %v, %v", picked, err)
			}
			step := capture.trace.StrategyStep
			if step == nil || len(step.Weights) != 1 || capture.trace.Candidates[step.Wanted].ID != "a" {
				t.Fatalf("strategy explanation used affinity accounting peers: %+v", step)
			}
			if gate != "rule" && weighted.stateCount != 1 {
				t.Fatalf("strategy explanation created a live pool: %d pools", weighted.stateCount)
			}
		})
	}
}

func TestRoutingClaudeThreadPinKeepsAvailabilityGates(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, gate := range []string{"ready", "reserve", "reserve-bypass", "kind", "disabled", "cooldown", "conflicting-pin"} {
			t.Run(fmt.Sprintf("stream=%t/%s", streaming, gate), func(t *testing.T) {
				manager := NewManager(nil, &WeightedRoundRobinSelector{}, nil)
				manager.RegisterExecutor(routingMergeExecutor{schedulerTestExecutor{provider: "claude"}})
				const model = "merge-thread-model"
				ownerID := t.Name() + "-owner"
				owner := &Auth{ID: ownerID, Provider: "claude", Attributes: map[string]string{AttributeAuthKind: AuthKindAPIKey, AttributeQuotaReserve: `{"weekly":30}`, AttributeQuotaReserveBypassKeys: `["` + quotaReserveBypassID + `"]`}}
				if gate == "disabled" {
					owner.Disabled = true
				}
				for _, a := range []*Auth{owner, {ID: t.Name() + "-other", Provider: "claude", Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth}}} {
					if _, err := manager.Register(WithSkipPersist(t.Context()), a); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
				}
				if gate == "reserve" || gate == "reserve-bypass" {
					snapshot, _ := manager.GetByID(ownerID)
					manager.ApplyQuotaReserveBars(snapshot, []QuotaReserveBar{{ID: "weekly", RemainingPercent: 20, ResetAt: time.Now().Add(time.Hour)}})
				}
				if gate == "cooldown" {
					manager.MarkResult(t.Context(), Result{AuthID: ownerID, Provider: "claude", Model: model, Error: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "temporarily unavailable"}})
				}
				cfg, err := internalconfig.ParseConfigBytes([]byte("routing:\n  rules:\n    - route-to: [" + t.Name() + "-other]\n"))
				if err != nil {
					t.Fatal(err)
				}
				manager.SetConfig(cfg)
				ctx, holder := routing.WithTraceHolder(t.Context())
				if gate == "kind" {
					ctx = withRequiredAuthKind(ctx, AuthKindOAuth)
				}
				opts := ex.Options{SourceFormat: sdktranslator.FormatClaude, Metadata: map[string]any{ex.CallerScopeMetadataKey: t.Name()}}
				if gate == "reserve-bypass" {
					opts.Metadata[ex.ClientKeyIDMetadataKey] = quotaReserveBypassID
				}
				if gate == "conflicting-pin" {
					opts.Metadata[ex.PinnedAuthMetadataKey] = t.Name() + "-other"
				}
				session.RememberClaudeThreadAuth(t.Name(), "msg_merge_thread", ownerID)
				req := ex.Request{Model: model, Payload: []byte(`{"thread":{"type":"continue","previous_message_id":"msg_merge_thread"}}`)}
				var payload []byte
				if streaming {
					var stream *ex.StreamResult
					stream, err = manager.ExecuteStream(ctx, []string{"claude"}, req, opts)
					if stream != nil {
						for chunk := range stream.Chunks {
							payload = append(payload, chunk.Payload...)
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					var response ex.Response
					response, err = manager.Execute(ctx, []string{"claude"}, req, opts)
					payload = response.Payload
				}
				if gate != "ready" && gate != "reserve-bypass" {
					if err == nil {
						t.Fatalf("thread pin bypassed %s: %s", gate, payload)
					}
					return
				}
				if err != nil || string(payload) != ownerID || holder.Last().DecidedBy != routing.DecidedByPin {
					t.Fatalf("thread owner = %s, error = %v, trace = %+v", payload, err, holder.Last())
				}
			})
		}
	}
}

func TestRoutingWeightedAffinityReloadPreservesPendingReservations(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit=%t", commit), func(t *testing.T) {
			weighted := &WeightedRoundRobinSelector{}
			manager := NewManager(nil, weighted, nil)
			const provider, model = "reload-pending", "reload-pending-model"
			manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
			var auths []*Auth
			for _, id := range []string{"a", "b"} {
				registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				auth, err := manager.Register(WithSkipPersist(t.Context()), &Auth{ID: id, Provider: provider, Status: StatusActive})
				if err != nil {
					t.Fatal(err)
				}
				auths = append(auths, auth)
			}
			ctx := context.WithValue(t.Context(), weightedAccountingNamespaceKey{}, "mixed")
			picked, pending, err := weighted.reserveForRequest(ctx, provider, model, ex.Options{}, auths, nil)
			if err != nil || pending == nil || picked.ID != "a" {
				t.Fatalf("initial reservation = %v, %v", picked, err)
			}
			defer pending.finish(false)
			state := pending.state
			generation := state.generation
			affinity := NewSessionAffinitySelector(weighted)
			defer affinity.Stop()
			manager.SetSelector(affinity)
			live, _, _, err := manager.pickNextMixed(t.Context(), []string{provider}, model, ex.Options{}, nil)
			if err != nil || live.ID != "b" {
				t.Fatalf("wrapped selection ignored the pending reservation: %v, %v", live, err)
			}
			manager.SetSelector(weighted)
			if current := weightedStateForTest(t, weighted, "mixed:"+model, "a", "b"); current != state || state.generation != generation || state.inFlight != 1 {
				t.Fatalf("reload replaced/reset the reserved pool: same=%t generation=%d inFlight=%d", current == state, state.generation, state.inFlight)
			}
			pending.finish(commit)
			if state.inFlight != 0 || len(state.pendingForTest()) != 0 {
				t.Fatalf("pending reservation did not settle on the retained pool: %+v", state)
			}
			if commit && state.hasCurrentForTest() {
				t.Fatalf("committed requests lost their balanced credit: %v", state.currentForTest())
			}
			if !commit && !reflect.DeepEqual(state.currentForTest(), map[string]int64{"a": 1, "b": -1}) {
				t.Fatalf("rollback lost the concurrent committed selection: %v", state.currentForTest())
			}
		})
	}
}
