package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func gsrFallbackTestManager(t *testing.T, model string, auths ...*Auth) *Manager {
	t.Helper()
	m := NewManager(nil, &GreedySoonestRecoverySelector{}, nil)
	for _, a := range auths {
		m.RegisterExecutor(&mixedProviderProbeExecutor{identifier: a.Provider})
		registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: model}})
		id := a.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func gsrFallbackQuota(signal string) QuotaState {
	return QuotaState{ObservedAt: time.Now(), Signals: map[string]string{signal: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)}}
}

func TestGSRMissingDataFallsBack(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		name := "single"
		if mixed {
			name = "mixed"
		}
		t.Run(name, func(t *testing.T) {
			a := &Auth{ID: "gsr-fallback-missing-" + name, Provider: "codex"}
			m := gsrFallbackTestManager(t, "gpt-audit", a)
			m.SetQuotaRecoveryRefresher(func(context.Context, *Auth, string) error { return errors.New("usage endpoint unavailable") })
			var err error
			if mixed {
				_, _, _, err = m.pickNextMixed(context.Background(), []string{"codex"}, "gpt-audit", cliproxyexecutor.Options{}, nil)
			} else {
				_, err = m.SelectAuth(context.Background(), "codex", "gpt-audit", cliproxyexecutor.Options{})
			}
			if err != nil {
				t.Errorf("request should fall back to RR: %v", err)
			}
			m.SetSelector(&RoundRobinSelector{})
			if _, err = m.SelectAuth(context.Background(), "codex", "gpt-audit", cliproxyexecutor.Options{}); err != nil {
				t.Fatalf("RR control failed: %v", err)
			}
		})
	}
}

func TestGSRPartialDataFallsBackAcrossEligiblePool(t *testing.T) {
	m := gsrFallbackTestManager(t, "gpt-audit", &Auth{ID: "gsr-fallback-known", Provider: "codex", Quota: gsrFallbackQuota("X-Codex-Primary-Reset-At")}, &Auth{ID: "gsr-fallback-unknown", Provider: "codex"})
	m.SetQuotaRecoveryRefresher(func(context.Context, *Auth, string) error { return errors.New("probe unavailable") })
	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		a, err := m.SelectAuth(context.Background(), "codex", "gpt-audit", cliproxyexecutor.Options{})
		if err != nil {
			t.Fatal(err)
		}
		counts[a.ID]++
	}
	if counts["gsr-fallback-unknown"] != 3 {
		t.Errorf("partial information should use RR over both eligible credentials: %v", counts)
	}
}

func TestGSRQuotaRefreshErrorsKeepCredentialsRoutable(t *testing.T) {
	for _, refreshErr := range []error{
		&Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage endpoint rate limited"},
		errors.New("quota transport: connection reset by peer"),
		fmt.Errorf("quota transport: %w", context.DeadlineExceeded),
		fmt.Errorf("quota transport: %w", context.Canceled),
	} {
		for _, mixed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/mixed=%t", refreshErr, mixed), func(t *testing.T) {
				m := gsrFallbackTestManager(t, "gpt-audit",
					&Auth{ID: "quota-refresh-first", Provider: "codex"},
					&Auth{ID: "quota-refresh-second", Provider: "codex"},
				)
				m.SetQuotaRecoveryRefresher(func(context.Context, *Auth, string) error { return refreshErr })
				counts := make(map[string]int)
				for range 4 {
					var picked *Auth
					var errPick error
					if mixed {
						picked, _, _, errPick = m.pickNextMixed(context.Background(), []string{"codex"}, "gpt-audit", cliproxyexecutor.Options{}, nil)
					} else {
						picked, errPick = m.SelectAuth(context.Background(), "codex", "gpt-audit", cliproxyexecutor.Options{})
					}
					if errPick != nil {
						t.Fatalf("quota refresh failure blocked routing: %v", errPick)
					}
					counts[picked.ID]++
				}
				if counts["quota-refresh-first"] != 2 || counts["quota-refresh-second"] != 2 {
					t.Fatalf("routing shares = %v, want both credentials routable", counts)
				}
			})
		}
	}
}

func TestGSRMixedPreservesProviderSpecificSignals(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			signal := "X-Codex-Primary-Reset-At"
			model := "gpt-audit"
			if provider == "claude" {
				signal = "Anthropic-Ratelimit-Unified-7d-Reset"
				model = "claude-sonnet-audit"
			}
			m := gsrFallbackTestManager(t, model, &Auth{ID: "gsr-fallback-mixed-" + provider, Provider: provider, Quota: gsrFallbackQuota(signal)})
			if _, err := m.SelectAuth(context.Background(), provider, model, cliproxyexecutor.Options{}); err != nil {
				t.Fatalf("single control: %v", err)
			}
			if _, _, _, err := m.pickNextMixed(context.Background(), []string{provider}, model, cliproxyexecutor.Options{}, nil); err != nil {
				t.Errorf("mixed request lost usable %s reset: %v", provider, err)
			}
		})
	}
}

func TestGSROpenCodeGLMUsesMonthly(t *testing.T) {
	a := &Auth{ID: "gsr-fallback-opencode", Provider: "openai-compatible-opencode-go", Quota: gsrFallbackQuota("monthly_reset_at")}
	if got := RecoveryTimestampForAuth(a, "deepseek-v4"); got.IsZero() {
		t.Fatal("non-GLM control failed")
	}
	if got := RecoveryTimestampForAuth(a, "glm-5.3"); got.IsZero() {
		t.Error("GLM request ignores OpenCode monthly reset")
	}
}

func TestGSRMissingDataWarns(t *testing.T) {
	hook := logtest.NewGlobal()
	defer log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	m := gsrFallbackTestManager(t, "gpt-audit", &Auth{ID: "gsr-fallback-log", Provider: "codex"})
	m.SetQuotaRecoveryRefresher(func(context.Context, *Auth, string) error { return errors.New("probe unavailable") })
	_, _ = m.SelectAuth(context.Background(), "codex", "gpt-audit", cliproxyexecutor.Options{})
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel {
			return
		}
	}
	t.Error("no warning emitted for missing recovery data / failed probe")
}

func TestGSRFallbackWarningIncludesRequestAndPoolDetails(t *testing.T) {
	hook := logtest.NewGlobal()
	defer log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	m := gsrFallbackTestManager(t, "gpt-warning", &Auth{ID: "gsr-fallback-warning", Provider: "codex"})
	ctx := internallogging.WithRequestID(context.Background(), "gsr-request-1")
	if _, err := m.SelectAuth(ctx, "codex", "gpt-warning", cliproxyexecutor.Options{}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range hook.AllEntries() {
		if entry.Level != log.WarnLevel || entry.Message != "GSR recovery data incomplete; using round-robin fallback" {
			continue
		}
		for key, want := range map[string]any{
			"request_id":     "gsr-request-1",
			"model":          "gpt-warning",
			"provider":       "codex",
			"reason":         "missing_or_unusable_recovery_data",
			"eligible_count": 1,
			"missing_count":  1,
		} {
			if entry.Data[key] != want {
				t.Fatalf("warning field %s = %#v, want %#v", key, entry.Data[key], want)
			}
		}
		return
	}
	t.Fatal("GSR fallback warning not found")
}

func TestGSRDisabledModelExcluded(t *testing.T) {
	m := gsrFallbackTestManager(t, "gpt-audit", &Auth{ID: "gsr-fallback-disabled", Provider: "codex", Quota: gsrFallbackQuota("X-Codex-Primary-Reset-At"), ModelStates: map[string]*ModelState{"gpt-audit": {Status: StatusDisabled}}}, &Auth{ID: "gsr-fallback-enabled", Provider: "codex", Quota: gsrFallbackQuota("X-Codex-Primary-Reset-At")})
	for i := 0; i < 4; i++ {
		a, err := m.SelectAuth(context.Background(), "codex", "gpt-audit", cliproxyexecutor.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if a.ID != "gsr-fallback-enabled" {
			t.Fatalf("selected disabled model: %s", a.ID)
		}
	}
}

func TestGSRDoesNotProbeDisabledModel(t *testing.T) {
	m := gsrFallbackTestManager(t, "gpt-audit", &Auth{ID: "gsr-fallback-disabled-probe", Provider: "codex", ModelStates: map[string]*ModelState{"gpt-audit": {Status: StatusDisabled}}}, &Auth{ID: "gsr-fallback-enabled-probe", Provider: "codex", Quota: gsrFallbackQuota("X-Codex-Primary-Reset-At")})
	probed := make(chan string, 2)
	m.SetQuotaRecoveryRefresher(func(_ context.Context, a *Auth, _ string) error { probed <- a.ID; return errors.New("unavailable") })
	if _, err := m.SelectAuth(context.Background(), "codex", "gpt-audit", cliproxyexecutor.Options{}); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-probed:
		t.Errorf("unnecessary synchronous probe for disabled model credential: %s", id)
	default:
	}
}

func TestGSRValidSingleProviderExecutes(t *testing.T) {
	m := gsrFallbackTestManager(t, "glm-audit", &Auth{ID: "gsr-fallback-happy", Provider: "zai", Quota: gsrFallbackQuota("weekly_reset_at")})
	response, err := m.Execute(context.Background(), []string{"zai"}, cliproxyexecutor.Request{Model: "glm-audit"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Payload) != "gsr-fallback-happy" {
		t.Fatalf("unexpected response %s", response.Payload)
	}
}

func TestGSRExecuteAddsSchedulingWeight(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "non-streaming"
		if stream {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			quota := gsrFallbackQuota("monthly_reset_at")
			a := &Auth{ID: "gsr-weight-a", Provider: "kiro", Attributes: map[string]string{AttributeWeight: "1000000"}, Quota: quota}
			b := &Auth{ID: "gsr-weight-b", Provider: "kiro", Quota: quota}
			m := gsrFallbackTestManager(t, "glm-audit", a, b)
			counts := map[string]int{}
			for i := 0; i < 300; i++ {
				req := cliproxyexecutor.Request{Model: "glm-audit"}
				if stream {
					result, errStream := m.ExecuteStream(context.Background(), []string{"kiro"}, req, cliproxyexecutor.Options{Stream: true})
					if errStream != nil {
						t.Fatal(errStream)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						counts[string(chunk.Payload)]++
					}
				} else {
					response, err := m.Execute(context.Background(), []string{"kiro"}, req, cliproxyexecutor.Options{})
					if err != nil {
						t.Fatal(err)
					}
					counts[string(response.Payload)]++
				}
			}
			if counts[a.ID] != 200 || counts[b.ID] != 100 {
				t.Fatalf("routing shares = %v, want 200:100 for GSR weights 2000000:1000001", counts)
			}
		})
	}
}

func TestGSRCodexExecuteWithValidSignals(t *testing.T) {
	m := gsrFallbackTestManager(t, "gpt-audit", &Auth{ID: "gsr-fallback-codex-execute", Provider: "codex", Quota: gsrFallbackQuota("X-Codex-Primary-Reset-At")})
	if _, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-audit"}, cliproxyexecutor.Options{}); err != nil {
		t.Errorf("execution with valid cached recovery failed: %v", err)
	}
}

func TestGSRFailedProbeCannotDemotePriority(t *testing.T) {
	m := gsrFallbackTestManager(t, "gpt-audit", &Auth{ID: "gsr-fallback-high", Provider: "codex", Attributes: map[string]string{"priority": "10"}}, &Auth{ID: "gsr-fallback-low", Provider: "codex", Quota: gsrFallbackQuota("X-Codex-Primary-Reset-At")})
	m.SetQuotaRecoveryRefresher(func(context.Context, *Auth, string) error { return errors.New("probe unavailable") })
	a, err := m.SelectAuth(context.Background(), "codex", "gpt-audit", cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "gsr-fallback-high" {
		t.Errorf("probe failure bypassed highest eligible priority: %s", a.ID)
	}
}

func TestGSRSessionSurvivesMissingUsage(t *testing.T) {
	model := "glm-audit"
	a := &Auth{ID: "gsr-fallback-session", Provider: "zai", Quota: gsrFallbackQuota("weekly_reset_at")}
	m := gsrFallbackTestManager(t, model, a)
	selector := NewSessionAffinitySelector(&GreedySoonestRecoverySelector{})
	defer selector.Stop()
	m.SetSelector(selector)
	opts := cliproxyexecutor.Options{Headers: http.Header{"Session_id": []string{"gsr-fallback-session-id"}}}
	if _, err := m.SelectAuth(context.Background(), "zai", model, opts); err != nil {
		t.Fatalf("initial session: %v", err)
	}
	_, err := m.UpdateQuotaObservation(context.Background(), a.ID, QuotaState{})
	if err != nil {
		t.Fatal(err)
	}
	refreshes := 0
	m.SetQuotaRecoveryRefresher(func(context.Context, *Auth, string) error {
		refreshes++
		return errors.New("probe unavailable")
	})
	if _, err := m.SelectAuth(context.Background(), "zai", model, opts); err != nil {
		t.Errorf("healthy session binding fails when quota data disappears: %v", err)
	}
	if refreshes != 0 {
		t.Fatalf("healthy session binding triggered %d quota refreshes", refreshes)
	}
}

func TestGSRNoRefresherExpiredAndMalformed(t *testing.T) {
	for _, raw := range []string{"", "invalid", "2020-01-01T00:00:00Z"} {
		t.Run(raw, func(t *testing.T) {
			m := gsrFallbackTestManager(t, "gpt-audit", &Auth{ID: "gsr-fallback-invalid", Provider: "codex", Quota: QuotaState{Signals: map[string]string{"X-Codex-Primary-Reset-At": raw}}})
			if _, err := m.SelectAuth(context.Background(), "codex", "gpt-audit", cliproxyexecutor.Options{}); err != nil {
				t.Errorf("no usable recovery data should fall back: %v", err)
			}
		})
	}
}

func TestGSRUnavailableModelIsNotRouted(t *testing.T) {
	model := "glm-audit"
	m := gsrFallbackTestManager(t, model, &Auth{ID: "gsr-fallback-unsupported", Provider: "zai", Quota: gsrFallbackQuota("weekly_reset_at")}, &Auth{ID: "gsr-fallback-supported", Provider: "zai", Quota: gsrFallbackQuota("weekly_reset_at")})
	registry.GetGlobalRegistry().RegisterClient("gsr-fallback-unsupported", "zai", []*registry.ModelInfo{{ID: "other-model"}})
	response, err := m.Execute(context.Background(), []string{"zai"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Payload) != "gsr-fallback-supported" {
		t.Fatalf("used unsupported credential: %s", response.Payload)
	}
}

func TestGSRRelativeResetRequiresObservation(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	a := &Auth{Provider: "codex", Quota: QuotaState{Signals: map[string]string{"X-Codex-Primary-Reset-After-Seconds": "60"}}}
	first, okFirst := authRecoveryTimestampAt(a, "codex", "gpt-audit", now)
	later, okLater := authRecoveryTimestampAt(a, "codex", "gpt-audit", now.Add(24*time.Hour))
	if okFirst && okLater && later.After(first) {
		t.Errorf("unanchored relative timestamp moves forever: first %v, later %v", first, later)
	}
}

func TestSchedulerGSRPartialDataFallsBackAcrossEligiblePool(t *testing.T) {
	model := "gpt-scheduler-gsr"
	registerSchedulerModels(t, "codex", model, "scheduler-known", "scheduler-unknown")
	scheduler := newSchedulerForTest(&GreedySoonestRecoverySelector{},
		&Auth{ID: "scheduler-known", Provider: "codex", Quota: gsrFallbackQuota("X-Codex-Primary-Reset-At")},
		&Auth{ID: "scheduler-unknown", Provider: "codex"},
	)
	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		picked, errPick := scheduler.pickSingleWithStrategy(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil, schedulerStrategyGreedySoonestRecovery)
		if errPick != nil {
			t.Fatalf("scheduler pick #%d: %v", i, errPick)
		}
		counts[picked.ID]++
	}
	if counts["scheduler-known"] != 3 || counts["scheduler-unknown"] != 3 {
		t.Fatalf("scheduler GSR fallback counts = %#v, want 3:3", counts)
	}
}

func TestGSRRefreshCancellationRemainsCancellation(t *testing.T) {
	m := gsrFallbackTestManager(t, "gpt-cancel-gsr",
		&Auth{ID: "gsr-cancel-a", Provider: "codex"},
		&Auth{ID: "gsr-cancel-b", Provider: "codex"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.SetQuotaRecoveryRefresher(func(context.Context, *Auth, string) error {
		cancel()
		return context.Canceled
	})
	_, errPick := m.SelectAuth(ctx, "codex", "gpt-cancel-gsr", cliproxyexecutor.Options{})
	if !errors.Is(errPick, context.Canceled) {
		t.Fatalf("selection error = %v, want context.Canceled", errPick)
	}
}
