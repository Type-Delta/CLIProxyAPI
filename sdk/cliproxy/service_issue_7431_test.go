package cliproxy

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
)

type noopServiceIssue7431Executor struct {
	provider string
}

func (e noopServiceIssue7431Executor) Identifier() string { return e.provider }

func (e noopServiceIssue7431Executor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e noopServiceIssue7431Executor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e noopServiceIssue7431Executor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e noopServiceIssue7431Executor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e noopServiceIssue7431Executor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestIssue7431_ServiceApplyManagerConfigPreservesWeightedSelectorAcrossAffinityReload(t *testing.T) {
	ctx := coreauth.WithSkipPersist(t.Context())
	const (
		provider = "issue-7431-svc-wrr"
		model    = "issue-7431-svc-model"
	)
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(noopServiceIssue7431Executor{provider: provider})
	t.Cleanup(func() {
		if selector, ok := manager.Selector().(coreauth.StoppableSelector); ok {
			selector.Stop()
		}
	})
	service := &Service{coreManager: manager}

	initialCfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy:        "weighted-round-robin",
			SessionAffinity: false,
		},
	}
	if !service.applyManagerConfig(ctx, configCommit{cfg: initialCfg, sequence: 1}) {
		t.Fatal("initial applyManagerConfig failed")
	}

	reg := registry.GetGlobalRegistry()
	var auths []*coreauth.Auth
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("svc-wrr-auth-%02d", i)
		a := &coreauth.Auth{ID: id, Provider: provider, Status: coreauth.StatusActive}
		auths = append(auths, a)
		reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		if _, errReg := manager.Register(ctx, a); errReg != nil {
			t.Fatalf("register %s: %v", id, errReg)
		}
		manager.RefreshSchedulerEntry(id)
	}
	t.Cleanup(func() {
		for _, a := range auths {
			reg.UnregisterClient(a.ID)
		}
	})

	var lastPickedID string
	manager.RegisterExecutor(&funcIssue7431Executor{
		provider: provider,
		onExec: func(a *coreauth.Auth) {
			lastPickedID = a.ID
		},
	})

	var pickedBefore []string
	for i := 0; i < 2; i++ {
		if _, errExec := manager.Execute(ctx, []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errExec != nil {
			t.Fatalf("execute %d failed: %v", i, errExec)
		}
		pickedBefore = append(pickedBefore, lastPickedID)
	}
	if pickedBefore[0] != auths[0].ID || pickedBefore[1] != auths[1].ID {
		t.Fatalf("picks before reload = %v, want [%s %s]", pickedBefore, auths[0].ID, auths[1].ID)
	}
	weightedBefore := coreauth.UnwrapWeightedRoundRobinSelector(manager.Selector())
	epochsBefore := make(map[string]uint64, len(auths))
	for _, auth := range manager.List() {
		epochsBefore[auth.ID] = auth.RegistrationEpoch
	}

	// Reload routing config changing session-affinity while keeping strategy: weighted-round-robin.
	reloadedCfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy:           "weighted-round-robin",
			SessionAffinity:    true,
			SessionAffinityTTL: "30m",
		},
	}
	if !service.applyManagerConfig(ctx, configCommit{cfg: reloadedCfg, sequence: 2}) {
		t.Fatal("reload applyManagerConfig failed")
	}
	if coreauth.UnwrapWeightedRoundRobinSelector(manager.Selector()) != weightedBefore {
		t.Fatal("affinity reload replaced the underlying weighted selector")
	}
	for _, auth := range manager.List() {
		if auth.RegistrationEpoch != epochsBefore[auth.ID] {
			t.Fatalf("affinity reload replaced registration epoch for %s", auth.ID)
		}
	}

	if _, errExec := manager.Execute(ctx, []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errExec != nil {
		t.Fatalf("execute after reload failed: %v", errExec)
	}
	if lastPickedID != auths[2].ID {
		t.Fatalf("pick after affinity config reload reset WRR to %q, want %q", lastPickedID, auths[2].ID)
	}
}

type funcIssue7431Executor struct {
	provider string
	onExec   func(*coreauth.Auth)
}

func (e funcIssue7431Executor) Identifier() string { return e.provider }

func (e funcIssue7431Executor) Execute(_ context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.onExec != nil {
		e.onExec(auth)
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID), Headers: http.Header{"X-Selected-Model": {req.Model}}}, nil
}

func (e funcIssue7431Executor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e funcIssue7431Executor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e funcIssue7431Executor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e funcIssue7431Executor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestIssue7431_ServiceWeightedReloadPublicRoutingParity(t *testing.T) {
	for _, route := range []string{"single", "mixed", "prefixed-alias", "codex-websocket", "xai-websocket"} {
		for _, transition := range []string{"enable", "disable", "ttl", "rebind-quota", "rebind-never"} {
			for _, rules := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/rules=%t", route, transition, rules), func(t *testing.T) {
					ctx := coreauth.WithSkipPersist(t.Context())
					providers := []string{"reload-wrr"}
					if route == "mixed" {
						providers = []string{"reload-wrr-a", "reload-wrr-b"}
					}
					if route == "codex-websocket" || route == "xai-websocket" {
						providers[0] = map[string]string{"codex-websocket": "codex", "xai-websocket": "xai"}[route]
						ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
					}
					model := "reload-wrr-model"
					if route == "prefixed-alias" {
						model = "scope/reload-wrr-public"
					}
					ids := make([]string, 4)
					for i := range ids {
						ids[i] = fmt.Sprintf("%s-auth-%02d", t.Name(), i)
					}
					initialAffinity := transition != "enable"
					configFor := func(affinity bool) *internalconfig.Config {
						cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "weighted-round-robin", SessionAffinity: affinity, SessionAffinityTTL: "1h"}}
						if rules {
							cfg.Routing.Rules = []internalconfig.RoutingRule{{RouteTo: ids[:3]}}
						}
						if route == "prefixed-alias" {
							cfg.OAuthModelAlias = map[string][]internalconfig.OAuthModelAlias{providers[0]: {{Name: "upstream-wrr-model", Alias: "reload-wrr-public"}}}
						}
						return cfg
					}
					manager := coreauth.NewManager(nil, nil, nil)
					control := coreauth.NewManager(nil, nil, nil)
					service := &Service{coreManager: manager}
					for _, svc := range []*Service{service, {coreManager: control}} {
						if !svc.applyManagerConfig(ctx, configCommit{cfg: configFor(initialAffinity), sequence: 1}) {
							t.Fatal("initial manager config failed")
						}
					}
					for _, m := range []*coreauth.Manager{manager, control} {
						for _, provider := range providers {
							m.RegisterExecutor(funcIssue7431Executor{provider: provider})
						}
						t.Cleanup(func() {
							if selector, ok := m.Selector().(coreauth.StoppableSelector); ok {
								selector.Stop()
							}
						})
					}
					for i, id := range ids {
						auth := &coreauth.Auth{ID: id, Provider: providers[i%len(providers)], Status: coreauth.StatusActive}
						if route == "prefixed-alias" {
							auth.Prefix = "scope"
						}
						if route == "codex-websocket" || route == "xai-websocket" {
							auth.Attributes = map[string]string{"websockets": "true"}
							if i == 3 {
								auth.Attributes = map[string]string{"websockets": "false", "weight": "1000"}
							}
						}
						registry.GetGlobalRegistry().RegisterClient(id, auth.Provider, []*registry.ModelInfo{{ID: model}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
						for _, m := range []*coreauth.Manager{manager, control} {
							if _, err := m.Register(ctx, auth.Clone()); err != nil {
								t.Fatal(err)
							}
							m.RefreshSchedulerEntry(id)
						}
					}
					run := func(m *coreauth.Manager, headers http.Header) string {
						t.Helper()
						response, err := m.Execute(ctx, providers, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Headers: headers})
						if err != nil {
							t.Fatal(err)
						}
						if route == "prefixed-alias" && response.Headers.Get("X-Selected-Model") != "upstream-wrr-model" {
							t.Fatalf("alias resolved to %q", response.Headers.Get("X-Selected-Model"))
						}
						return string(response.Payload)
					}
					for i := range 2 {
						if got, want := run(manager, nil), run(control, nil); got != want || got != ids[i] {
							t.Fatalf("initial pick = %s, control = %s, want %s", got, want, ids[i])
						}
					}
					if initialAffinity {
						for range 3 {
							headers := http.Header{"X-Session-Id": {"warm-before-reload"}}
							if got, want := run(manager, headers), run(control, headers); got != want || got != ids[2] {
								t.Fatalf("warmed session = %s, control = %s, want %s", got, want, ids[2])
							}
						}
						if len(providers) == 1 {
							binding, status := manager.LookupSessionAffinity(providers[0], model, "warm-before-reload")
							if status != "bound" || binding.ID != ids[2] {
								t.Fatalf("single-provider binding = %v, %s", binding, status)
							}
							if _, status := manager.LookupSessionAffinity("mixed", model, "warm-before-reload"); status != "unbound" {
								t.Fatalf("single-provider cache moved into mixed namespace: %s", status)
							}
						}
					}
					weighted := coreauth.UnwrapWeightedRoundRobinSelector(manager.Selector())
					reloaded := configFor(transition != "disable")
					switch transition {
					case "ttl":
						reloaded.Routing.SessionAffinityTTL = "30m"
					case "rebind-quota":
						reloaded.Routing.SessionAffinityRebindOn = "quota"
					case "rebind-never":
						reloaded.Routing.SessionAffinityRebindOn = "never"
					}
					if !service.applyManagerConfig(ctx, configCommit{cfg: reloaded, sequence: 2}) {
						t.Fatal("reload failed")
					}
					if coreauth.UnwrapWeightedRoundRobinSelector(manager.Selector()) != weighted {
						t.Fatal("reload replaced the weighted selector")
					}
					dry, err := manager.DryRunRoute(ctx, routing.DryRunRequest{Model: model, DownstreamWebsocket: route == "codex-websocket" || route == "xai-websocket"})
					if err != nil || dry.StrategyStep == nil || len(dry.StrategyStep.Next) != 10 {
						t.Fatalf("dry run = %+v, %v", dry, err)
					}
					for i, index := range dry.StrategyStep.Next {
						headers := http.Header{"X-Session-Id": {fmt.Sprintf("fresh-after-reload-%d", i)}}
						got, want := run(manager, headers), run(control, headers)
						if got != want || got != dry.Candidates[index].ID {
							t.Fatalf("pick %d after reload = %s, unchanged control = %s, dry run = %s", i, got, want, dry.Candidates[index].ID)
						}
					}
					if transition != "disable" && len(providers) == 1 {
						binding, status := manager.LookupSessionAffinity(providers[0], model, "fresh-after-reload-9")
						if status != "bound" || binding == nil {
							t.Fatalf("provider cache binding missing after reload: %v, %s", binding, status)
						}
						if _, status := manager.LookupSessionAffinity("mixed", model, "fresh-after-reload-9"); status != "unbound" {
							t.Fatalf("reload changed the affinity cache namespace: %s", status)
						}
					}
				})
			}
		}
	}
}
