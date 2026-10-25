package management

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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

func newGSRProbeFixture(t *testing.T) *gsrProbeFixture {
	t.Helper()
	fixture := &gsrProbeFixture{status: http.StatusOK}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		fixture.seen = append(fixture.seen, r.Method+" https://"+r.Host+r.URL.RequestURI())
		fixture.headers = append(fixture.headers, r.Header.Clone())
		status, body := fixture.status, fixture.body
		fixture.mu.Unlock()
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
	claudeFableBody := fmt.Sprintf(`{"seven_day":{"utilization":5,"resets_at":%q},"limits":[{"kind":"weekly_scoped","percent":20,"is_active":true,"resets_at":%q,"scope":{"model":{"display_name":"fable"}}}]}`, reset.Add(time.Hour).Format(time.RFC3339), reset.Format(time.RFC3339))

	tests := []struct {
		name        string
		provider    string
		attributes  map[string]string
		model       string
		body        string
		wantRequest string
		wantSignal  string
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
		},
		{
			name:        "opencode go compatibility credential",
			provider:    "openai-compatible-opencode-go",
			attributes:  map[string]string{"api_key": "dummy-opencode-key"},
			model:       "glm-5.3",
			body:        openCodeBody,
			wantRequest: "GET https://opencode.ai/zen/go/v1/usage",
			wantSignal:  "monthly_reset_at",
		},
		{
			name:        "codex credential keeps the codex probe",
			provider:    "codex",
			attributes:  map[string]string{"api_key": "dummy-codex-key"},
			model:       "gpt-5",
			body:        codexBody,
			wantRequest: "GET https://chatgpt.com/backend-api/wham/usage",
			wantSignal:  "X-Codex-Primary-Reset-At",
		},
		{
			name:        "claude credential keeps the claude probe",
			provider:    "claude",
			attributes:  map[string]string{"api_key": "dummy-claude-key"},
			model:       "claude-sonnet-4-5",
			body:        claudeBody,
			wantRequest: "GET https://api.anthropic.com/api/oauth/usage",
			wantSignal:  "Anthropic-Ratelimit-Unified-7d-Reset",
		},
		{
			name:        "claude fable credential persists the fable window",
			provider:    "claude",
			attributes:  map[string]string{"api_key": "dummy-claude-key"},
			model:       "claude-fable-5-1",
			body:        claudeFableBody,
			wantRequest: "GET https://api.anthropic.com/api/oauth/usage",
			wantSignal:  "Anthropic-Ratelimit-Unified-7d_oi-Reset",
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
			if len(updated.Quota.Signals) != 1 {
				t.Fatalf("signals = %v, want only %s", updated.Quota.Signals, test.wantSignal)
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
