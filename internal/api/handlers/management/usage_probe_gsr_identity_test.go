package management

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
)

// gsrProbeFixture serves every provider hostname locally. RefreshQuotaForRouting
// then runs through the real APICall transport, credential injection and quota
// parsing without any outbound request.
type gsrProbeFixture struct {
	server  *httptest.Server
	mu      sync.Mutex
	seen    []string
	headers []http.Header
	status  int
	body    string
}

func newGSRProbeFixture(t *testing.T, handlers ...http.Handler) *gsrProbeFixture {
	t.Helper()
	fixture := &gsrProbeFixture{status: http.StatusOK}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		fixture.seen = append(fixture.seen, r.Method+" https://"+r.Host+r.URL.RequestURI())
		fixture.headers = append(fixture.headers, r.Header.Clone())
		status, body := fixture.status, fixture.body
		fixture.mu.Unlock()
		if len(handlers) > 0 {
			handlers[0].ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(fixture.server.Close)

	originalTransport := http.DefaultTransport
	baseTransport, ok := originalTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport type = %T, want *http.Transport", originalTransport)
	}
	localTransport := baseTransport.Clone()
	localTransport.TLSClientConfig = &tls.Config{
		RootCAs:    fixture.server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
		ServerName: "example.com",
	}
	localTransport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, fixture.server.Listener.Addr().String())
	}
	http.DefaultTransport = localTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	return fixture
}

func (f *gsrProbeFixture) setResponse(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func (f *gsrProbeFixture) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func gsrProbeHandler(t *testing.T, provider string, attributes map[string]string) (*Handler, *coreauth.Auth) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	auth, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:         "gsr-probe-" + provider,
		Provider:   provider,
		Attributes: attributes,
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	return &Handler{authManager: manager}, auth
}

// TestRefreshQuotaForRoutingResolvesProbeIdentity pins the endpoint, parser and
// persisted recovery signal to the credential's usage provider identity rather
// than to its adapter, and keeps credentials away from an unrelated upstream.
func TestRefreshQuotaForRoutingResolvesProbeIdentity(t *testing.T) {
	reset := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	zaiBody := fmt.Sprintf(`{"code":200,"success":true,"data":{"limits":[{"unit":6,"number":1,"nextResetTime":%d}]}}`, reset.UnixMilli())
	openCodeBody := fmt.Sprintf(`{"usage":{"monthly":{"percent":32,"resetsAt":%q}}}`, reset.Format(time.RFC3339))
	codexBody := fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":10,"window_minutes":10080,"reset_at":%d}}}`, reset.Unix())
	claudeBody := fmt.Sprintf(`{"seven_day":{"utilization":10,"resets_at":%q}}`, reset.Format(time.RFC3339))
	claudeFableBody := fmt.Sprintf(`{"seven_day":{"utilization":5,"resets_at":%q},"limits":[{"kind":"weekly_scoped","percent":20,"is_active":true,"resets_at":%q,"scope":{"model":{"display_name":"Fable 5"}}}]}`, reset.Add(time.Hour).Format(time.RFC3339), reset.Format(time.RFC3339))
	claudeCloudBody := fmt.Sprintf(`{"seven_day":{"utilization":5,"resets_at":%q},"iguana_necktie":{"utilization":95,"limit_dollars":50,"resets_at":%q}}`, reset.Format(time.RFC3339), reset.Add(time.Hour).Format(time.RFC3339))

	tests := []struct {
		name        string
		provider    string
		attributes  map[string]string
		model       string
		body        string
		wantRequest string
		wantSignal  string
		wantBars    int
	}{
		{
			name:        "z.ai compatibility credential",
			provider:    "openai-compatible-z.ai",
			attributes:  map[string]string{"api_key": "dummy-zai-key"},
			model:       "glm-5.3",
			body:        zaiBody,
			wantRequest: "GET https://api.z.ai/api/monitor/usage/quota/limit",
			wantSignal:  "weekly_reset_at",
		},
		{
			name:        "usage_probe override beats a generic compatibility adapter",
			provider:    "openai-compatibility",
			attributes:  map[string]string{"usage_probe": "zai", "api_key": "dummy-zai-key"},
			model:       "glm-5.3",
			body:        zaiBody,
			wantRequest: "GET https://api.z.ai/api/monitor/usage/quota/limit",
			wantSignal:  "weekly_reset_at",
		},
		{
			name:        "usage_probe override beats the codex adapter",
			provider:    "codex",
			attributes:  map[string]string{"usage_probe": "opencode-go", "api_key": "dummy-opencode-key"},
			model:       "deepseek-v4",
			body:        openCodeBody,
			wantRequest: "GET https://opencode.ai/zen/go/v1/usage",
			wantSignal:  "monthly_reset_at",
			wantBars:    1,
		},
		{
			name:        "opencode go compatibility credential",
			provider:    "openai-compatible-opencode-go",
			attributes:  map[string]string{"api_key": "dummy-opencode-key"},
			model:       "glm-5.3",
			body:        openCodeBody,
			wantRequest: "GET https://opencode.ai/zen/go/v1/usage",
			wantSignal:  "monthly_reset_at",
			wantBars:    1,
		},
		{
			name:        "codex credential keeps the codex probe",
			provider:    "codex",
			attributes:  map[string]string{"api_key": "dummy-codex-key"},
			model:       "gpt-5",
			body:        codexBody,
			wantRequest: "GET https://chatgpt.com/backend-api/wham/usage",
			wantSignal:  "X-Codex-Primary-Reset-At",
			wantBars:    1,
		},
		{
			name:        "claude credential keeps the claude probe",
			provider:    "claude",
			attributes:  map[string]string{"api_key": "dummy-claude-key"},
			model:       "claude-sonnet-4-5",
			body:        claudeBody,
			wantRequest: "GET https://api.anthropic.com/api/oauth/usage",
			wantSignal:  "Anthropic-Ratelimit-Unified-7d-Reset",
			wantBars:    1,
		},
		{
			name:        "claude fable credential persists the fable window",
			provider:    "claude",
			attributes:  map[string]string{"api_key": "dummy-claude-key"},
			model:       "claude-fable-5-1",
			body:        claudeFableBody,
			wantRequest: "GET https://api.anthropic.com/api/oauth/usage",
			wantSignal:  "Anthropic-Ratelimit-Unified-7d_oi-Reset",
			wantBars:    2,
		},
		{
			name:        "claude cloud session credits are separate from Fable",
			provider:    "claude",
			attributes:  map[string]string{"api_key": "dummy-claude-key"},
			model:       "claude-fable-5-1",
			body:        claudeCloudBody,
			wantRequest: "GET https://api.anthropic.com/api/oauth/usage",
			wantSignal:  "Anthropic-Ratelimit-Unified-7d-Reset",
			wantBars:    1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newGSRProbeFixture(t)
			fixture.setResponse(http.StatusOK, test.body)
			handler, auth := gsrProbeHandler(t, test.provider, test.attributes)

			if errRefresh := handler.RefreshQuotaForRouting(context.Background(), auth, test.model); errRefresh != nil {
				t.Fatalf("RefreshQuotaForRouting() error = %v", errRefresh)
			}
			requests := fixture.requests()
			wantRequests := []string{test.wantRequest}
			switch coreauth.QuotaRecoveryProviderForAuth(auth) {
			case "codex":
				wantRequests = append(wantRequests, "GET "+codexResetCreditsURL)
			case "claude":
				wantRequests = append(wantRequests, "GET "+claudeResetGrantsQuotaURL)
			}
			if !slices.Equal(requests, wantRequests) {
				t.Fatalf("probe requests = %#v, want %#v", requests, wantRequests)
			}
			updated, ok := handler.authManager.GetByID(auth.ID)
			if !ok || updated == nil {
				t.Fatal("updated auth not found")
			}
			if got := updated.Quota.Signals[test.wantSignal]; got != reset.Format(time.RFC3339) {
				t.Fatalf("%s = %q, want %q (all signals %v)", test.wantSignal, got, reset.Format(time.RFC3339), updated.Quota.Signals)
			}
			bars := 0
			for signal := range updated.Quota.Signals {
				if strings.HasPrefix(signal, "gsr-quota:") {
					bars++
				}
			}
			if bars != test.wantBars {
				t.Fatalf("signals = %v, want %d quota bars", updated.Quota.Signals, test.wantBars)
			}
		})
	}
}

func TestRefreshQuotaForRoutingObservesOpenCodeExhaustedWindow(t *testing.T) {
	fixture := newGSRProbeFixture(t)
	reset := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	for _, usage := range []string{`"status":"rate_limited"`, `"status":"rate_limited","percent":25`} {
		t.Run(usage, func(t *testing.T) {
			fixture.setResponse(http.StatusOK, fmt.Sprintf(`{"usage":{"monthly":{%s,"resetsAt":%q}}}`, usage, reset.Format(time.RFC3339)))
			handler, auth := gsrProbeHandler(t, "openai-compatible-opencode-go", map[string]string{"api_key": "dummy-opencode-key"})
			if errRefresh := handler.RefreshQuotaForRouting(context.Background(), auth, "test-model"); errRefresh != nil {
				t.Fatal(errRefresh)
			}
			current, _ := handler.authManager.GetByID(auth.ID)
			want := fmt.Sprintf("0:%d", reset.UnixMilli())
			if got := current.Quota.Signals["gsr-quota:opencode-go:opencode-go-monthly"]; got != want {
				t.Fatalf("exhausted usage observation = %q, want %q", got, want)
			}
		})
	}
}

// TestRefreshQuotaForRoutingRejectsUnsupportedProbe pins that an unrecognized
// explicit usage probe never falls back to the adapter's upstream, and that a
// credential without any quota recovery identity fails before any probe.
func TestRefreshQuotaForRoutingRejectsUnsupportedProbe(t *testing.T) {
	tests := []struct {
		name       string
		provider   string
		attributes map[string]string
	}{
		{
			name:       "unknown explicit usage probe",
			provider:   "codex",
			attributes: map[string]string{"usage_probe": "kimi", "api_key": "dummy-codex-key"},
		},
		{
			name:       "explicit value naming an unrelated adapter",
			provider:   "claude",
			attributes: map[string]string{"usage_probe": "codex", "api_key": "dummy-claude-key"},
		},
		{
			name:       "provider without a quota recovery probe",
			provider:   "gemini",
			attributes: map[string]string{"api_key": "dummy-gemini-key"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newGSRProbeFixture(t)
			fixture.setResponse(http.StatusOK, `{"usage":{"monthly":{"percent":32,"resetsAt":"2030-01-01T00:00:00Z"}}}`)
			handler, auth := gsrProbeHandler(t, test.provider, test.attributes)

			if errRefresh := handler.RefreshQuotaForRouting(context.Background(), auth, "glm-5.3"); errRefresh == nil {
				t.Fatal("RefreshQuotaForRouting() error = nil, want unsupported provider error")
			}
			if requests := fixture.requests(); len(requests) != 0 {
				t.Fatalf("unsupported credential reached the network: %#v", requests)
			}
			updated, _ := handler.authManager.GetByID(auth.ID)
			if updated != nil && len(updated.Quota.Signals) != 0 {
				t.Fatalf("unsupported credential persisted signals: %v", updated.Quota.Signals)
			}
		})
	}
}

func TestRefreshQuotaForRoutingKeepsCachedResetAndUsageTogether(t *testing.T) {
	fixture := newGSRProbeFixture(t)
	fixture.setResponse(http.StatusOK, `{"rate_limit":{"secondary_window":{"limit_window_seconds":604800,"reset_after_seconds":3600}}}`)
	handler, auth := gsrProbeHandler(t, "codex", map[string]string{"api_key": "dummy-codex-key"})
	now := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Minute)
	handler.quotaCache = newAPICallQuotaCache(func() time.Time { return now })
	old := coreauth.QuotaState{ObservedAt: now.Add(-time.Hour), Signals: map[string]string{
		"X-Codex-Primary-Used-Percent": "99", "X-Codex-Primary-Reset-After-Seconds": "10",
	}}
	if _, errUpdate := handler.authManager.UpdateQuotaObservation(context.Background(), auth.ID, old); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	observedAt := now
	wantReset := now.Add(time.Hour).Format(time.RFC3339)
	for range 2 {
		current, _ := handler.authManager.GetByID(auth.ID)
		if errRefresh := handler.RefreshQuotaForRouting(context.Background(), current, "gpt-5"); errRefresh != nil {
			t.Fatal(errRefresh)
		}
		current, _ = handler.authManager.GetByID(auth.ID)
		if !current.Quota.ObservedAt.Equal(observedAt) || len(current.Quota.Signals) != 1 || current.Quota.Signals["X-Codex-Primary-Reset-At"] != wantReset {
			t.Fatalf("probe observation = %+v, want only the reset at %s from %v", current.Quota, wantReset, observedAt)
		}
		now = now.Add(time.Minute)
	}
	if got := len(fixture.requests()); got != 2 {
		t.Fatalf("provider requests = %d, want one usage and one manual-reset request", got)
	}
}

func TestRefreshQuotaForRoutingRejectsReplacedOAuthBackoff(t *testing.T) {
	ctx := context.Background()
	oldStarted, releaseOld := make(chan struct{}), make(chan struct{})
	var release sync.Once
	reset := time.Now().Add(time.Hour).Unix()
	fixture := newGSRProbeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer old-account":
			close(oldStarted)
			<-releaseOld
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		case "Bearer new-account":
			if r.URL.Path == "/backend-api/wham/usage" {
				fmt.Fprintf(w, `{"rate_limit":{"primary_window":{"used_percent":20,"reset_at":%d}}}`, reset)
			} else {
				fmt.Fprint(w, `{"credits":[]}`)
			}
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(func() { release.Do(func() { close(releaseOld) }) })
	manager := coreauth.NewManager(nil, nil, nil)
	old, errRegister := manager.Register(ctx, &coreauth.Auth{ID: "oauth-backoff", Provider: "codex",
		Metadata: map[string]any{"access_token": "old-account", "refresh_token": "old-grant"}})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	handler := &Handler{authManager: manager}
	oldDone := make(chan error, 1)
	go func() { oldDone <- handler.RefreshQuotaForRouting(ctx, old, "gpt-5") }()
	select {
	case <-oldStarted:
	case errProbe := <-oldDone:
		t.Fatalf("original probe never hit the provider: %v", errProbe)
	}
	replacement := old.Clone()
	replacement.Metadata["access_token"] = "new-account"
	replacement.Metadata["refresh_token"] = "new-grant"
	current, errUpdate := manager.Update(ctx, replacement)
	release.Do(func() { close(releaseOld) })
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if errProbe := <-oldDone; errProbe == nil {
		t.Fatal("original probe should have received HTTP 429")
	}
	if errProbe := handler.RefreshQuotaForRouting(ctx, current, "gpt-5"); errProbe != nil {
		t.Fatalf("replacement probe failed after %d provider requests: %v", len(fixture.requests()), errProbe)
	}
}

// TestRefreshQuotaForRoutingRejectsUnusableWindows pins that malformed or
// missing reset data is refused instead of persisted as a recovery deadline.
func TestRefreshQuotaForRoutingRejectsUnusableWindows(t *testing.T) {
	tests := []struct {
		name       string
		provider   string
		attributes map[string]string
		model      string
		body       string
	}{
		{
			name:       "opencode monthly without a reset",
			provider:   "openai-compatible-opencode-go",
			attributes: map[string]string{"api_key": "dummy-opencode-key"},
			model:      "glm-5.3",
			body:       `{"usage":{"monthly":{"percent":32}}}`,
		},
		{
			name:       "opencode monthly with a malformed reset",
			provider:   "openai-compatible-opencode-go",
			attributes: map[string]string{"api_key": "dummy-opencode-key"},
			model:      "glm-5.3",
			body:       `{"usage":{"monthly":{"percent":32,"resetsAt":"not-a-timestamp"}}}`,
		},
		{
			name:       "opencode monthly reset already expired",
			provider:   "codex",
			attributes: map[string]string{"usage_probe": "opencode-go", "api_key": "dummy-opencode-key"},
			model:      "glm-5.3",
			body:       fmt.Sprintf(`{"usage":{"monthly":{"percent":32,"resetsAt":%q}}}`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)),
		},
		{
			name:       "z.ai limit without a reset",
			provider:   "openai-compatible-z.ai",
			attributes: map[string]string{"api_key": "dummy-zai-key"},
			model:      "glm-5.3",
			body:       `{"code":200,"success":true,"data":{"limits":[{"unit":6,"number":1}]}}`,
		},
		{
			name:       "codex response without windows",
			provider:   "codex",
			attributes: map[string]string{"api_key": "dummy-codex-key"},
			model:      "gpt-5",
			body:       `{"rate_limit":{"allowed":true}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newGSRProbeFixture(t)
			fixture.setResponse(http.StatusOK, test.body)
			handler, auth := gsrProbeHandler(t, test.provider, test.attributes)

			if errRefresh := handler.RefreshQuotaForRouting(context.Background(), auth, test.model); errRefresh == nil {
				t.Fatal("RefreshQuotaForRouting() error = nil, want unusable-window error")
			}
			if requests := fixture.requests(); len(requests) != 1 {
				t.Fatalf("probe requests = %#v, want a single probed request", requests)
			}
			updated, _ := handler.authManager.GetByID(auth.ID)
			if updated != nil && len(updated.Quota.Signals) != 0 {
				t.Fatalf("unusable window persisted signals: %v", updated.Quota.Signals)
			}
		})
	}
}

func TestRoutingGSRProbeRecoveryAcrossSharedKeyEntries(t *testing.T) {
	fixture := newGSRProbeFixture(t)
	reset := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	manager := coreauth.NewManager(nil, &coreauth.GreedySoonestRecoverySelector{}, nil)
	handler := &Handler{authManager: manager}
	modelName := "glm-shared-key-recovery"
	providers := []string{"openai-compatible-zai-a", "openai-compatible-zai-b"}
	var auths []*coreauth.Auth
	for i, provider := range providers {
		fixture.setResponse(http.StatusOK, fmt.Sprintf(`{"code":200,"success":true,"data":{"limits":[{"unit":6,"number":1,"nextResetTime":%d,"percentage":%d}]}}`, reset.UnixMilli(), 90-80*i))
		a, err := manager.Register(context.Background(), &coreauth.Auth{ID: provider, Provider: provider, Status: coreauth.StatusActive, Attributes: map[string]string{"usage_probe": "zai", "api_key": "dummy-shared-key"}})
		if err != nil {
			t.Fatal(err)
		}
		auths = append(auths, a)
		manager.RegisterExecutor(&refreshRecordExecutor{provider: provider})
		registry.GetGlobalRegistry().RegisterClient(a.ID, provider, []*registry.ModelInfo{{ID: modelName}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
		// A repeated probe exercises the quota cache as well as persistence per entry.
		for range 2 {
			if err := handler.RefreshQuotaForRouting(context.Background(), a, modelName); err != nil {
				t.Fatal(err)
			}
		}
	}
	calls := len(fixture.requests())
	if calls != 2 {
		t.Fatalf("probe calls = %d, want one cached lookup per credential: %v", calls, fixture.requests())
	}
	var refreshes atomic.Int32
	manager.SetQuotaRecoveryRefresher(func(ctx context.Context, a *coreauth.Auth, model string) error {
		refreshes.Add(1)
		return handler.RefreshQuotaForRouting(ctx, a, model)
	})
	for cycle := range 4 {
		dry, err := manager.DryRunRoute(context.Background(), routing.DryRunRequest{Model: modelName})
		if err != nil {
			t.Fatal(err)
		}
		if !dry.PickPredicted || dry.StrategyStep == nil || dry.StrategyStep.Applied != routing.StrategyGreedySoonestRecovery {
			t.Fatalf("cycle=%d dry=%+v", cycle, dry)
		}
		for _, row := range dry.StrategyStep.Weights {
			if row.RecoveryAtMs != reset.UnixMilli() || row.RecoverySource != "signal:weekly_reset_at" {
				t.Fatalf("cycle=%d recovery=%+v", cycle, row)
			}
			want := 0.1
			if dry.Candidates[row.Index].ID == providers[1] {
				want = 0.9
			}
			if row.RemainingQuotaFraction == nil || *row.RemainingQuotaFraction != want {
				t.Fatalf("cycle=%d quota fraction = %v, want %.1f", cycle, row.RemainingQuotaFraction, want)
			}
		}
		if cycle == 0 && dry.Candidates[dry.Pick].ID != providers[1] {
			t.Fatalf("first dry-run pick = %q, want %q", dry.Candidates[dry.Pick].ID, providers[1])
		}
		ctx, holder := routing.WithTraceHolder(context.Background())
		if _, err := manager.Execute(ctx, providers, cliproxyexecutor.Request{Model: modelName}, cliproxyexecutor.Options{}); err != nil {
			t.Fatal(err)
		}
		live := holder.Last()
		if live == nil || live.Candidates[live.Pick].ID != dry.Candidates[dry.Pick].ID {
			t.Fatalf("cycle=%d dry=%+v live=%+v", cycle, dry, live)
		}
		for _, a := range auths {
			current, _ := manager.GetByID(a.ID)
			if current.Quota.Signals["weekly_reset_at"] != reset.Format(time.RFC3339) {
				t.Fatalf("cycle=%d %s lost persisted recovery: %+v", cycle, a.ID, current.Quota)
			}
		}
	}
	if refreshes.Load() != 0 || len(fixture.requests()) != calls {
		t.Fatalf("traffic forced quota refresh: refreshes=%d HTTP calls=%d", refreshes.Load(), len(fixture.requests()))
	}
	t.Logf("two shared-key entries retained weekly recovery across four dry-run/Execute cycles; initial HTTP calls=%d; subsequent refreshes=%d", calls, refreshes.Load())
}
