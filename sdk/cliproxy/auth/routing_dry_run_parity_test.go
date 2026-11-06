package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
)

func TestRoutingDryRunGSRFallbackCursorParity(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, affinity := range []bool{false, true} {
			t.Run(fmt.Sprintf("mixed=%t/affinity=%t", mixed, affinity), func(t *testing.T) {
				model := "gsr-fallback-cursor"
				providers := []string{"openai-compatible-zai-a", "openai-compatible-zai-b", "openai-compatible-opencode-go"}
				if !mixed {
					providers = []string{providers[0]}
				}
				pool := []*Auth{
					{ID: "a-gated", Provider: providers[0], Status: StatusActive, Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour)},
					{ID: "b", Provider: providers[0], Status: StatusActive, Quota: gsrFallbackQuota("weekly_reset_at")},
					{ID: "c", Provider: providers[len(providers)-1], Status: StatusActive, Quota: gsrFallbackQuota("weekly_reset_at"), Attributes: map[string]string{"usage_probe": "zai", "weight": "2"}},
				}
				// Keep the third provider in the universe while gating it out of the pool.
				pool[0].Provider = providers[len(providers)-1]
				if mixed {
					pool[2].Provider = providers[1]
				}
				m := gsrFallbackTestManager(t, model, pool...)
				if affinity {
					s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: m.Selector(), TTL: time.Hour})
					defer s.Stop()
					m.SetSelector(s)
				}
				pick := func() (*Auth, *routing.Trace) {
					ctx, holder := routing.WithTraceHolder(context.Background())
					var a *Auth
					var err error
					if mixed {
						a, _, _, err = m.pickNextMixed(ctx, providers, model, ex.Options{}, nil)
					} else {
						a, _, err = m.pickNext(ctx, providers[0], model, ex.Options{}, nil)
					}
					if err != nil {
						t.Fatal(err)
					}
					return a, holder.Last()
				}
				// Prime smooth GSR credits before switching to its persistent RR fallback.
				pick()
				missing, _ := m.GetByID("c")
				missing.Quota = QuotaState{}
				if _, err := m.Update(WithSkipPersist(context.Background()), missing); err != nil {
					t.Fatal(err)
				}
				previous := ""
				for cycle := range 4 {
					dry, err := m.DryRunRoute(context.Background(), routing.DryRunRequest{Model: model})
					if err != nil {
						t.Fatal(err)
					}
					step := dry.StrategyStep
					if step == nil || step.Applied != routing.StrategyRoundRobin {
						t.Fatalf("cycle=%d step=%+v", cycle, step)
					}
					lastID := ""
					if step.Last >= 0 {
						lastID = dry.Candidates[step.Last].ID
					}
					if lastID != previous {
						t.Errorf("cycle=%d dry last=%q want previous live=%q", cycle, lastID, previous)
					}
					for _, row := range step.Weights {
						if row.Credit != 0 {
							t.Errorf("round-robin row has smooth credit: %+v", row)
						}
					}
					live, trace := pick()
					if dry.Candidates[step.Wanted].ID != live.ID {
						t.Errorf("cycle=%d dry=%s live=%s", cycle, dry.Candidates[step.Wanted].ID, live.ID)
					}
					liveLast := ""
					if trace.StrategyStep.Last >= 0 {
						liveLast = trace.Candidates[trace.StrategyStep.Last].ID
					}
					if liveLast != previous {
						t.Errorf("cycle=%d live last=%q want=%q", cycle, liveLast, previous)
					}
					previous = live.ID
				}
			})
		}
	}
}

func TestRoutingDryRunGSRProbeRecoverySurvivesResults(t *testing.T) {
	model := "glm-probe-recovery-parity"
	pool := []*Auth{
		{ID: "probe-a", Provider: "openai-compatible-zai-a", Status: StatusActive, Attributes: map[string]string{"usage_probe": "zai", "api_key": "shared-test-key"}},
		{ID: "probe-b", Provider: "openai-compatible-zai-b", Status: StatusActive, Attributes: map[string]string{"usage_probe": "zai", "api_key": "shared-test-key"}},
	}
	m := gsrFallbackTestManager(t, model, pool...)
	// Refresh through the same persistence API as RefreshQuotaForRouting.
	quota := QuotaState{ObservedAt: time.Now(), Signals: map[string]string{"weekly_reset_at": "2090-01-01T00:00:00Z"}}
	var refreshes atomic.Int32
	for _, a := range pool {
		if _, err := m.UpdateQuotaObservation(context.Background(), a.ID, quota); err != nil {
			t.Fatal(err)
		}
	}
	m.SetQuotaRecoveryRefresher(func(ctx context.Context, a *Auth, _ string) error {
		refreshes.Add(1)
		_, err := m.UpdateQuotaObservation(ctx, a.ID, quota)
		return err
	})
	providers := []string{pool[0].Provider, pool[1].Provider}
	for cycle := range 4 {
		cached, _ := m.GetByID(pool[0].ID)
		t.Logf("cycle=%d persisted probe-a weekly_reset_at=%q", cycle, cached.Quota.Signals["weekly_reset_at"])
		dry, err := m.DryRunRoute(context.Background(), routing.DryRunRequest{Model: model})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range dry.StrategyStep.Weights {
			t.Logf("cycle=%d credential=%s recovery_at_ms=%d", cycle, dry.Candidates[row.Index].ID, row.RecoveryAtMs)
			if row.RecoveryAtMs == 0 || row.RecoverySource != "signal:weekly_reset_at" {
				t.Errorf("cycle=%d missing probe recovery: %+v", cycle, row)
			}
		}
		ctx, holder := routing.WithTraceHolder(context.Background())
		live, _, _, err := m.pickNextMixed(ctx, providers, model, ex.Options{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !dry.PickPredicted || dry.Candidates[dry.Pick].ID != live.ID || holder.Last().StrategyStep.Applied != routing.StrategyGreedySoonestRecovery {
			t.Errorf("cycle=%d dry=%+v live=%s step=%+v", cycle, dry.StrategyStep, live.ID, holder.Last().StrategyStep)
		}
		m.MarkResult(context.Background(), Result{AuthID: live.ID, Provider: live.Provider, Model: model, Success: true, CompletedResponse: true})
	}
	if refreshes.Load() != 0 {
		t.Errorf("traffic erased cached probe observations, triggering %d refreshes", refreshes.Load())
	}
	// The cached observations must still exist on both registered entries.
	for _, a := range pool {
		current, _ := m.GetByID(a.ID)
		if current.Quota.Signals["weekly_reset_at"] == "" {
			t.Errorf("%s lost persisted probe reset", a.ID)
		}
	}
}

func TestRoutingSchedulerGSRFallbackCursorAndCredits(t *testing.T) {
	pool := telemetryPool()
	s := newAuthScheduler(&GreedySoonestRecoverySelector{})
	s.rebuild(pool)
	if _, err := s.pickSingle(context.Background(), "zai", "", ex.Options{}, nil); err != nil {
		t.Fatal(err)
	}
	pool[1].Quota = QuotaState{}
	s.upsertAuth(pool[1])
	previous := ""
	for cycle := range 4 {
		_, capture := telemetryCapture()
		for _, a := range pool {
			capture.candidateIndex(a)
		}
		shard := s.providers["zai"].modelShards[""]
		step, sim := s.scheduledStrategySimulation(capture, []*modelScheduler{shard}, false, 0, nil, s.strategy, []string{"zai"}, "")
		if step.Applied != routing.StrategyRoundRobin {
			t.Fatalf("step=%+v", step)
		}
		for _, row := range step.Weights {
			if row.Credit != 0 {
				t.Errorf("round-robin credit=%d", row.Credit)
			}
		}
		predicted := sim.next()
		ctx, live := telemetryCapture()
		for _, a := range pool {
			live.candidateIndex(a)
		}
		picked, err := s.pickSingle(ctx, "zai", "", ex.Options{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if picked.ID != predicted.ID {
			t.Errorf("cycle=%d dry=%s live=%s", cycle, predicted.ID, picked.ID)
		}
		for _, c := range []*routingCapture{capture, live} {
			traced := step
			if c == live {
				traced = live.trace.StrategyStep
			}
			last := ""
			if traced.Last >= 0 {
				last = c.trace.Candidates[traced.Last].ID
			}
			if last != previous {
				t.Errorf("cycle=%d last=%q want=%q", cycle, last, previous)
			}
		}
		previous = picked.ID
	}
}

func TestRoutingProbeRecoverySurvivesAdapterHeaders(t *testing.T) {
	for _, probe := range []string{"zai", "opencode-go"} {
		for _, adapter := range []string{"openai-compatible-pool", "codex"} {
			t.Run(probe+"/"+adapter, func(t *testing.T) {
				signal := "weekly_reset_at"
				if probe == "opencode-go" {
					signal = "monthly_reset_at"
				}
				quota := gsrFallbackQuota(signal)
				m := NewManager(nil, nil, nil)
				a, err := m.Register(WithSkipPersist(context.Background()), &Auth{ID: "probe", Provider: adapter, Attributes: map[string]string{"usage_probe": probe}, Quota: quota})
				if err != nil {
					t.Fatal(err)
				}
				ctx := internallogging.WithResponseHeadersHolder(context.Background())
				internallogging.SetResponseHeaders(ctx, http.Header{"X-Codex-Primary-Reset-At": []string{"1893456000"}, "Retry-After": []string{"60"}})
				m.MarkResult(ctx, Result{AuthID: a.ID, Provider: adapter, Model: "model", Success: true, CompletedResponse: true})
				current, _ := m.GetByID(a.ID)
				if current.Quota.Signals[signal] != quota.Signals[signal] || !current.Quota.ObservedAt.Equal(quota.ObservedAt) || len(current.Quota.Signals) != 1 {
					t.Fatalf("probe observation replaced by adapter headers: %+v", current.Quota)
				}
			})
		}
	}
}
