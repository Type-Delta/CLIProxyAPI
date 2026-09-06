package management

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// quotaProxy is an HTTP proxy so tests exercise the same transport selection
// used by APICall while keeping provider traffic local and observable.
type quotaProxy struct {
	server *httptest.Server
	count  atomic.Int64
	mu     sync.Mutex
	seen   []string
	status int
	header http.Header
	body   string
	gate   <-chan struct{}
}

func newQuotaProxy(t *testing.T) *quotaProxy {
	t.Helper()
	p := &quotaProxy{status: http.StatusOK, body: `{"ok":true}`}
	p.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.count.Add(1)
		p.mu.Lock()
		p.seen = append(p.seen, r.Method+" "+r.URL.String()+" "+r.Header.Get("X-Test-Parameter"))
		status, header, body, gate := p.status, p.header.Clone(), p.body, p.gate
		p.mu.Unlock()
		if gate != nil {
			<-gate
		}
		for key, values := range header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	// Route the provider's HTTPS hostnames to this local TLS server while
	// retaining the real APICall transport and URL classifier.
	originalTransport := http.DefaultTransport
	baseTransport, ok := originalTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport type = %T, want *http.Transport", originalTransport)
	}
	localTransport := baseTransport.Clone()
	localTransport.TLSClientConfig = &tls.Config{
		RootCAs:    p.server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
		ServerName: "example.com",
	}
	localTransport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, errSplit := net.SplitHostPort(address)
		if errSplit == nil {
			switch host {
			case "api.anthropic.com", "chatgpt.com", "api.x.ai", "cli-chat-proxy.grok.com", "daily-cloudcode-pa.googleapis.com", "daily-cloudcode-pa.sandbox.googleapis.com", "cloudcode-pa.googleapis.com", "example.invalid":
				address = p.server.Listener.Addr().String()
			}
		}
		return baseTransport.DialContext(ctx, network, address)
	}
	http.DefaultTransport = localTransport
	t.Cleanup(p.server.Close)
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	return p
}

func (p *quotaProxy) setResponse(status int, header http.Header, body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status, p.header, p.body = status, header, body
}

func (p *quotaProxy) setGate(gate <-chan struct{}) {
	p.mu.Lock()
	p.gate = gate
	p.mu.Unlock()
}

func quotaHandler(proxy *quotaProxy) *Handler {
	manager := coreauth.NewManager(nil, nil, nil)
	for _, fixture := range []struct {
		index    string
		provider string
	}{
		{index: "credential-a-claude", provider: "claude"},
		{index: "credential-b-claude", provider: "claude"},
		{index: "credential-a-codex", provider: "codex"},
		{index: "credential-a-xai", provider: "xai"},
		{index: "credential-a-antigravity", provider: "antigravity"},
	} {
		auth := &coreauth.Auth{ID: fixture.index, Index: fixture.index, Provider: fixture.provider, Metadata: map[string]any{"access_token": "test-token"}}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			panic(errRegister)
		}
	}
	return &Handler{cfg: &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: ""}}, authManager: manager, quotaCache: newAPICallQuotaCache(time.Now)}
}

func quotaCall(t *testing.T, h *Handler, proxy *quotaProxy, authIndex, method, target, parameter, data string) apiCallResponse {
	t.Helper()
	payload := map[string]any{
		"auth_index": authIndex,
		"method":     method,
		"url":        target,
		"proxy_url":  "",
		"header":     map[string]string{"X-Test-Parameter": parameter},
		"data":       data,
	}
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	c, _ := gin.CreateTestContext(recorder)
	c.Request = request
	h.APICall(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("APICall HTTP status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response apiCallResponse
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode APICall response: %v", errDecode)
	}
	return response
}

func TestAPICallQuotaRefreshSharesConcurrentClients(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	release := make(chan struct{})
	proxy.setGate(release)
	const callers = 12
	responses := make(chan apiCallResponse, callers)
	var wg sync.WaitGroup
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses <- quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, "https://api.anthropic.com/api/oauth/usage", "same", "")
		}()
	}
	deadline := time.After(2 * time.Second)
	for {
		if proxy.count.Load() == 1 {
			break
		}
		select {
		case <-deadline:
			releaseGate()
			wg.Wait()
			t.Fatalf("provider call count = %d, want one in-flight call", proxy.count.Load())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	releaseGate()
	wg.Wait()
	close(responses)
	for response := range responses {
		if response.StatusCode != http.StatusOK {
			t.Errorf("response status_code = %d, want 200", response.StatusCode)
		}
	}
	if got := proxy.count.Load(); got != 1 {
		t.Fatalf("provider call count = %d, want one", got)
	}
}

func TestAPICallQuotaRefreshCacheSeparatesCredentialsAndParameters(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	target := "https://api.anthropic.com/api/oauth/usage"
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "one", "")
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "one", "")
	quotaCall(t, h, proxy, "credential-b-claude", http.MethodGet, target, "one", "")
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target+"?window=other", "one", "")
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "two", "")
	if got := proxy.count.Load(); got != 4 {
		t.Fatalf("provider call count = %d, want four isolated cache keys", got)
	}
}

func TestAPICallQuotaRefreshDoesNotCacheMutations(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	target := "https://api.anthropic.com/api/oauth/usage"
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodPost, target, "same", `{"attempt":1}`)
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodPost, target, "same", `{"attempt":1}`)
	if got := proxy.count.Load(); got != 2 {
		t.Fatalf("provider call count = %d, want two uncached mutation calls", got)
	}
}

func TestAPICallCodexConsumeInvalidatesCredentialQuotaCache(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	usageURL := "https://chatgpt.com/backend-api/wham/usage"
	consumeURL := "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
	quotaCall(t, h, proxy, "credential-a-codex", http.MethodGet, usageURL, "same", "")
	quotaCall(t, h, proxy, "credential-a-codex", http.MethodGet, usageURL, "same", "")
	quotaCall(t, h, proxy, "credential-a-codex", http.MethodPost, consumeURL, "same", `{"model":"gpt-5"}`)
	quotaCall(t, h, proxy, "credential-a-codex", http.MethodGet, usageURL, "same", "")
	if got := proxy.count.Load(); got != 3 {
		t.Fatalf("provider call count = %d, want initial usage, consume, and fresh post-consume usage", got)
	}
}

func TestAPICallCodexConsumeKeepsActiveProviderBackoff(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	usageURL := "https://chatgpt.com/backend-api/wham/usage"
	consumeURL := "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
	proxy.setResponse(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"120"}}, `{"error":"slow down"}`)
	quotaCall(t, h, proxy, "credential-a-codex", http.MethodGet, usageURL, "same", "")
	proxy.setResponse(http.StatusOK, nil, `{"ok":"consumed"}`)
	quotaCall(t, h, proxy, "credential-a-codex", http.MethodPost, consumeURL, "same", `{"model":"gpt-5"}`)
	quotaCall(t, h, proxy, "credential-a-codex", http.MethodGet, usageURL, "same", "")
	if got := proxy.count.Load(); got != 2 {
		t.Fatalf("provider call count after consume = %d, want two; successful consume must preserve active 429 backoff", got)
	}
}

func TestAPICallQuotaRefresh429BackoffPreventsFurtherProviderCalls(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	proxy.setResponse(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"120"}}, `{"error":"slow down"}`)
	target := "https://api.anthropic.com/api/oauth/usage"
	first := quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "same", "")
	second := quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "same", "")
	if first.StatusCode != http.StatusTooManyRequests || second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("statuses = %d, %d, want both 429", first.StatusCode, second.StatusCode)
	}
	if got := proxy.count.Load(); got != 1 {
		t.Fatalf("provider call count = %d, want one while retry-after is active", got)
	}
}

func TestAPICallQuotaRefresh429BackoffDoesNotBlockOtherCredentials(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	proxy.setResponse(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"120"}}, `{"error":"slow down"}`)
	target := "https://api.anthropic.com/api/oauth/usage"
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "same", "")
	quotaCall(t, h, proxy, "credential-b-claude", http.MethodGet, target, "same", "")
	if got := proxy.count.Load(); got != 2 {
		t.Fatalf("provider call count = %d, want each credential to have an independent backoff", got)
	}
}

func TestAPICallQuotaRefresh429BackoffIsSharedAcrossCredentialEndpoints(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	proxy.setResponse(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"180"}}, `{"error":"slow down"}`)
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, "https://api.anthropic.com/api/oauth/usage", "same", "")
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, "https://api.anthropic.com/api/oauth/profile", "same", "")
	if got := proxy.count.Load(); got != 1 {
		t.Fatalf("provider call count = %d, want endpoint sibling blocked by credential/provider backoff", got)
	}
}

func TestAPICallQuotaRefresh429BackoffIsSharedAcrossAntigravityFallbackHosts(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	proxy.setResponse(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"120"}}, `{"error":"slow down"}`)
	body := `{"project":"test"}`
	quotaCall(t, h, proxy, "credential-a-antigravity", http.MethodPost, "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", "same", body)
	quotaCall(t, h, proxy, "credential-a-antigravity", http.MethodPost, "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", "same", body)
	if got := proxy.count.Load(); got != 1 {
		t.Fatalf("provider call count = %d, want fallback host blocked by shared provider backoff", got)
	}
}

func TestAPICallQuotaRefresh401DoesNotCreateManagementBan(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	h.localPassword = "management-secret"
	h.envSecret = h.localPassword
	h.allowRemoteOverride = true
	proxy.setResponse(http.StatusUnauthorized, nil, `{"error":"expired oauth"}`)
	for i := 0; i < 10; i++ {
		response := quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, "https://api.anthropic.com/api/oauth/usage", strconv.Itoa(i), "")
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("call %d status_code = %d, want 401", i, response.StatusCode)
		}
	}
	if allowed, status, message := h.AuthenticateManagementKey("127.0.0.1", true, "management-secret"); !allowed || status != 0 || message != "" {
		t.Fatalf("provider failures affected management authentication: allowed=%v status=%d message=%q", allowed, status, message)
	}
}

func TestAPICallQuotaRefreshResponseCooldownIsOneMinute(t *testing.T) {
	proxy := newQuotaProxy(t)
	now := time.Unix(1000, 0)
	h := quotaHandler(proxy)
	h.quotaCache = newAPICallQuotaCache(func() time.Time { return now })
	target := "https://api.anthropic.com/api/oauth/usage"
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "same", "")
	now = now.Add(59 * time.Second)
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "same", "")
	if got := proxy.count.Load(); got != 1 {
		t.Fatalf("provider call count at 59s = %d, want one cached result", got)
	}
	now = now.Add(time.Second)
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "same", "")
	if got := proxy.count.Load(); got != 2 {
		t.Fatalf("provider call count at exactly one minute = %d, want two", got)
	}
}

func TestAPICallXaiPaidHealthChatMutationIsCachedOnlyForHealthEndpoint(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	target := "https://api.x.ai/v1/chat/completions"
	data := `{"model":"grok-4.5","messages":[{"role":"user","content":"ping"}],"max_tokens":1,"stream":false}`
	quotaCall(t, h, proxy, "credential-a-xai", http.MethodPost, target, "health", data)
	quotaCall(t, h, proxy, "credential-a-xai", http.MethodPost, target, "health", data)
	quotaCall(t, h, proxy, "credential-a-xai", http.MethodPost, target, "chat", `{"model":"grok-4.5","messages":[{"role":"user","content":"real request"}]}`)
	if got := proxy.count.Load(); got != 2 {
		t.Fatalf("provider call count = %d, want paid-health ping cached and ordinary chat uncached", got)
	}
}

func TestAPICallQuotaRefresh429RetryAfterDateDoesNotUseNormalCooldown(t *testing.T) {
	proxy := newQuotaProxy(t)
	now := time.Unix(1000, 0).UTC()
	h := quotaHandler(proxy)
	h.quotaCache = newAPICallQuotaCache(func() time.Time { return now })
	retryAt := now.Add(10 * time.Second).Format(http.TimeFormat)
	proxy.setResponse(http.StatusTooManyRequests, http.Header{"Retry-After": []string{retryAt}}, `{"error":"slow down"}`)
	target := "https://api.anthropic.com/api/oauth/usage"
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "same", "")
	now = now.Add(20 * time.Second)
	proxy.setResponse(http.StatusOK, nil, `{"ok":"fresh"}`)
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "same", "")
	if got := proxy.count.Load(); got != 2 {
		t.Fatalf("provider call count after Retry-After date = %d, want two", got)
	}
}

func TestAPICallQuotaRefreshShortRetryAfterExpiresBeforeNormalResultCooldown(t *testing.T) {
	proxy := newQuotaProxy(t)
	now := time.Unix(1000, 0).UTC()
	h := quotaHandler(proxy)
	h.quotaCache = newAPICallQuotaCache(func() time.Time { return now })
	proxy.setResponse(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"10"}}, `{"error":"slow down"}`)
	target := "https://api.anthropic.com/api/oauth/usage"
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "same", "")
	now = now.Add(9 * time.Second)
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "same", "")
	now = now.Add(2 * time.Second)
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "same", "")
	if got := proxy.count.Load(); got != 2 {
		t.Fatalf("provider call count after short Retry-After = %d, want two; 429 must not extend to one-minute result TTL", got)
	}
}

func TestAPICallQuotaRefresh429InvalidRetryAfterFallsBackToThreeMinutes(t *testing.T) {
	proxy := newQuotaProxy(t)
	now := time.Unix(1000, 0).UTC()
	h := quotaHandler(proxy)
	h.quotaCache = newAPICallQuotaCache(func() time.Time { return now })
	proxy.setResponse(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"nonsense"}}, `{"error":"slow down"}`)
	target := "https://api.anthropic.com/api/oauth/usage"
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "same", "")
	now = now.Add(3*time.Minute - time.Nanosecond)
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "same", "")
	if got := proxy.count.Load(); got != 1 {
		t.Fatalf("provider call count before fallback expiry = %d, want one", got)
	}
	now = now.Add(time.Nanosecond)
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "same", "")
	if got := proxy.count.Load(); got != 2 {
		t.Fatalf("provider call count at fallback expiry = %d, want two", got)
	}
}

func TestAPICallQuotaRefresh429AbsentRetryAfterFallsBackToThreeMinutes(t *testing.T) {
	proxy := newQuotaProxy(t)
	now := time.Unix(1000, 0).UTC()
	h := quotaHandler(proxy)
	h.quotaCache = newAPICallQuotaCache(func() time.Time { return now })
	proxy.setResponse(http.StatusTooManyRequests, nil, `{"error":"slow down"}`)
	target := "https://api.anthropic.com/api/oauth/usage"
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "same", "")
	now = now.Add(3 * time.Minute)
	quotaCall(t, h, proxy, "credential-a-claude", http.MethodGet, target, "same", "")
	if got := proxy.count.Load(); got != 2 {
		t.Fatalf("provider call count at fallback expiry = %d, want two", got)
	}
}

func TestAPICallQuotaRefreshCanceledCallerDoesNotStrandFlight(t *testing.T) {
	cache := newAPICallQuotaCache(time.Now)
	key := quotaCacheKey{requestHash: "request", authHash: "auth", backoffHash: "provider"}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	fn := func(context.Context) quotaCallOutcome {
		calls.Add(1)
		close(started)
		<-release
		return quotaCallOutcome{hasResponse: true, response: apiCallResponse{StatusCode: http.StatusOK}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	leaderDone := make(chan quotaCallOutcome, 1)
	go func() { leaderDone <- cache.do(ctx, key, fn) }()
	<-started
	cancel()
	waiterDone := make(chan quotaCallOutcome, 1)
	go func() { waiterDone <- cache.do(context.Background(), key, fn) }()
	close(release)
	select {
	case <-leaderDone:
	case <-time.After(time.Second):
		t.Fatal("canceled leader did not finish")
	}
	select {
	case outcome := <-waiterDone:
		if !outcome.successfulResponse() {
			t.Fatalf("waiter outcome = %+v, want successful shared result", outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter remained stranded after canceled leader")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider function calls = %d, want one shared flight", got)
	}
}

func TestAPICallQuotaRefreshUnknownEndpointRemainsGeneric(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	target := "https://example.invalid/v1/models"
	quotaCall(t, h, proxy, "credential-a", http.MethodGet, target, "same", "")
	quotaCall(t, h, proxy, "credential-a", http.MethodGet, target, "same", "")
	if got := proxy.count.Load(); got != 2 {
		t.Fatalf("provider call count = %d, want generic endpoint calls uncached", got)
	}
}

func TestAPICallQuotaRefreshTruncated429PreservesBackoff(t *testing.T) {
	for _, tokenRefresh := range []bool{false, true} {
		t.Run(strconv.FormatBool(tokenRefresh), func(t *testing.T) {
			proxy := newQuotaProxy(t)
			h := quotaHandler(proxy)
			now := time.Unix(1000, 0)
			h.quotaCache = newAPICallQuotaCache(func() time.Time { return now })
			proxy.setResponse(http.StatusTooManyRequests, http.Header{"Retry-After": {"120"}, "Content-Length": {"100"}}, "short")
			payload := `{"auth_index":"credential-a-claude","method":"GET","url":"https://api.anthropic.com/api/oauth/usage"}`
			wantStatus := http.StatusBadGateway
			if tokenRefresh {
				previousURL := antigravityOAuthTokenURL
				antigravityOAuthTokenURL = proxy.server.URL
				t.Cleanup(func() { antigravityOAuthTokenURL = previousURL })
				auth := h.authByIndex("credential-a-antigravity")
				auth.Metadata = map[string]any{"refresh_token": "test-refresh"}
				if _, errUpdate := h.authManager.Update(context.Background(), auth); errUpdate != nil {
					t.Fatal(errUpdate)
				}
				payload = `{"auth_index":"credential-a-antigravity","method":"POST","url":"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary","header":{"Authorization":"Bearer $TOKEN$"},"data":"{}"}`
				wantStatus = http.StatusBadRequest
			}
			call := func() {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(payload))
				c.Request.Header.Set("Content-Type", "application/json")
				h.APICall(c)
				if recorder.Code != wantStatus {
					t.Fatalf("HTTP status = %d, want %d: %s", recorder.Code, wantStatus, recorder.Body.String())
				}
			}
			call()
			now = now.Add(61 * time.Second)
			call()
			if got := proxy.count.Load(); got != 1 {
				t.Fatalf("provider calls during Retry-After = %d, want 1", got)
			}
			now = now.Add(59 * time.Second)
			call()
			if got := proxy.count.Load(); got != 2 {
				t.Fatalf("provider calls at Retry-After expiry = %d, want 2", got)
			}
		})
	}
}

func TestAPICallQuotaRefreshCachedDatePreservesServerClockOffset(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	now := time.Unix(1000, 0).UTC()
	h.quotaCache = newAPICallQuotaCache(func() time.Time { return now })
	providerTime := now.Add(5 * time.Second)
	proxy.setResponse(http.StatusOK, http.Header{"Date": {providerTime.Format(http.TimeFormat)}}, `{"ok":true}`)
	url := "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"
	quotaCall(t, h, proxy, "credential-a-antigravity", http.MethodPost, url, "same", `{}`)
	for _, elapsed := range []time.Duration{30 * time.Second, 29 * time.Second} {
		now = now.Add(elapsed)
		response := quotaCall(t, h, proxy, "credential-a-antigravity", http.MethodPost, url, "same", `{}`)
		date, errParse := http.ParseTime(http.Header(response.Header).Get("Date"))
		if errParse != nil || date.Sub(now) != 5*time.Second {
			t.Fatalf("cached Date = %v, error = %v; want server offset +5s at %v", date, errParse, now)
		}
	}
	if got := proxy.count.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}
