package management

import (
	"context"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestQuotaHistoryProviderMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	service := &quotaHistoryFakeService{}
	h.SetAnalyticsService(service)
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/quota-history", strings.NewReader(`{"provider":"claude","samples":[{"auth_index":"credential-a-codex","window_id":"seven-day","remaining_percent":30}]}`))
	h.PostQuotaHistory(ctx)
	ctx.Writer.WriteHeaderNow()
	t.Logf("cross-provider submission status=%d stored=%+v", response.Code, service.samples)
	if response.Code != 400 {
		t.Errorf("expected cross-provider auth index rejection, got %d", response.Code)
	}
}
func TestKiroCollectorReusesPanelURL(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	http.DefaultTransport.(*http.Transport).DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, proxy.server.Listener.Addr().String())
	}
	proxy.setResponse(200, nil, `{"usageBreakdownList":[{"resourceType":"CREDIT","currentUsage":20,"usageLimit":100}]}`)
	auth := h.authByIndex("credential-a-codex")
	auth.ID = "kiro-fixture"
	auth.Index = "kiro-fixture"
	auth.Provider = "kiro"
	if _, err := h.authManager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	panelURL := "https://codewhisperer.us-east-1.amazonaws.com/getUsageLimits?origin=AI_EDITOR&resourceType=AGENTIC_REQUEST&isEmailRequired=true"
	quotaCall(t, h, proxy, "kiro-fixture", "GET", panelURL, "panel", "")
	samples, err := h.collectQuotaHistoryCredential(context.Background(), auth, "kiro", testQuotaHistoryService{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("panel URL=%s auto URL=%s calls=%d samples=%d", panelURL, kiroUsageLimitsURL(auth), proxy.count.Load(), len(samples))
	if proxy.count.Load() != 1 {
		t.Error("collector duplicated cached Kiro base usage request because query parameter order differs")
	}
}

func TestQuotaHistoryBaseCacheIdentityPreservesPartitions(t *testing.T) {
	h := quotaHandler(nil)
	auth := h.authByIndex("credential-a-codex")
	parse := func(value string) *url.URL {
		u, err := url.Parse(value)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	key := func(method, rawURL, proxy, body string, a *coreauth.Auth) string {
		return buildQuotaCacheKey(h, a, method, parse(rawURL), proxy, nil, body).baseHash
	}
	base := key("GET", "https://example.invalid/usage?a=1&b=2", "", "", auth)
	if got := key("GET", "https://example.invalid/usage?b=2&a=1", "", "", auth); got != base {
		t.Fatal("query order changed identity")
	}
	other := h.authByIndex("credential-b-claude")
	for _, got := range []string{key("GET", "https://example.invalid/usage?a=3&b=2", "", "", auth), key("POST", "https://example.invalid/usage?a=1&b=2", "", "", auth), key("GET", "https://example.invalid/usage?a=1&b=2", "http://proxy.invalid", "", auth), key("GET", "https://example.invalid/usage?a=1&b=2", "", "body", auth), key("GET", "https://example.invalid/usage?a=1&b=2", "", "", other)} {
		if got == base {
			t.Fatal("distinct request shared base identity")
		}
	}
}
