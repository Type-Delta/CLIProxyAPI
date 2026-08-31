package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type viewerRouteReader struct {
	query   model.Query
	summary model.Summary
	events  model.EventPage
}

func (r *viewerRouteReader) Summary(_ context.Context, query model.Query) (model.Summary, error) {
	r.query = query
	return r.summary, nil
}
func (r *viewerRouteReader) Timeseries(_ context.Context, query model.Query) (model.Timeseries, error) {
	r.query = query
	return model.Timeseries{}, nil
}
func (r *viewerRouteReader) Dimensions(_ context.Context, query model.Query) (model.DimensionPage, error) {
	r.query = query
	return model.DimensionPage{}, nil
}
func (r *viewerRouteReader) Events(_ context.Context, query model.Query) (model.EventPage, error) {
	r.query = query
	return r.events, nil
}
func (r *viewerRouteReader) Leaderboard(_ context.Context, query model.Query) (model.LeaderboardPage, error) {
	r.query = query
	return model.LeaderboardPage{}, nil
}

type viewerRouteMaintenance struct{}

func (viewerRouteMaintenance) Start(context.Context, cpauk.MaintenanceRequest) (model.JobStatus, error) {
	return model.JobStatus{}, cpauk.ErrUnavailable
}
func (viewerRouteMaintenance) Status(context.Context, string) (model.JobStatus, error) {
	return model.JobStatus{}, cpauk.ErrUnavailable
}
func (viewerRouteMaintenance) Cancel(context.Context, string) error { return cpauk.ErrUnavailable }

type viewerRouteService struct{ reader *viewerRouteReader }

func (s *viewerRouteService) Observer() coreusage.Plugin     { return nil }
func (s *viewerRouteService) Reader() cpauk.Reader           { return s.reader }
func (s *viewerRouteService) Maintenance() cpauk.Maintenance { return viewerRouteMaintenance{} }
func (s *viewerRouteService) Capabilities() cpauk.Capabilities {
	return model.Capabilities{Supported: true, Enabled: true, Available: true, State: model.StateReady}
}
func (s *viewerRouteService) Health() cpauk.Health { return model.Health{} }
func (s *viewerRouteService) Reconfigure(cpauk.Config) cpauk.ReconfigureResult {
	return cpauk.ReconfigureResult{}
}
func (s *viewerRouteService) Retry(context.Context) error { return nil }
func (s *viewerRouteService) Close(context.Context) error { return nil }

func newViewerRouteFixture(t *testing.T, options AnalyticsViewerSecurityOptions) (*gin.Engine, *managementHandlers.AnalyticsViewerStore, managementHandlers.ViewerCreateResponse, *viewerRouteReader) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store, err := managementHandlers.NewAnalyticsViewerStore("")
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(managementHandlers.ViewerCreateRequest{
		KeyID: strings.Repeat("a", 64), AllowedViews: []string{"summary", "timeseries", "events"},
		ExpiresAt: time.Now().UTC().Add(time.Hour), Label: "shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	reader := &viewerRouteReader{summary: model.Summary{ProxyRequests: 2, Tokens: model.TokenUsage{Total: 30}}}
	server := &Server{engine: gin.New(), analytics: &viewerRouteService{reader: reader}}
	if err = server.RegisterAnalyticsViewerRoutes(store, options); err != nil {
		t.Fatal(err)
	}
	return server.engine, store, created, reader
}

func TestViewerSessionRequiresAuthenticatedTransport(t *testing.T) {
	tests := []struct {
		name       string
		options    AnalyticsViewerSecurityOptions
		url        string
		remoteAddr string
		host       string
		forwarded  string
		want       int
	}{
		{name: "direct TLS", url: "https://cpa.test/v0/analytics/viewer/session", remoteAddr: "198.51.100.2:1234", want: http.StatusNoContent},
		{name: "trusted proxy", options: AnalyticsViewerSecurityOptions{TrustedProxyCIDRs: []string{"203.0.113.0/24"}}, url: "http://cpa.test/v0/analytics/viewer/session", remoteAddr: "203.0.113.7:1234", forwarded: "https", want: http.StatusNoContent},
		{name: "spoofed forwarded proto", url: "http://cpa.test/v0/analytics/viewer/session", remoteAddr: "198.51.100.2:1234", forwarded: "https", want: http.StatusUnauthorized},
		{name: "plain production HTTP", url: "http://cpa.test/v0/analytics/viewer/session", remoteAddr: "198.51.100.2:1234", want: http.StatusUnauthorized},
		{name: "explicit loopback development", options: AnalyticsViewerSecurityOptions{AllowLoopbackHTTP: true}, url: "http://localhost/v0/analytics/viewer/session", remoteAddr: "127.0.0.1:1234", host: "localhost", want: http.StatusNoContent},
		{name: "loopback peer with public host", options: AnalyticsViewerSecurityOptions{AllowLoopbackHTTP: true}, url: "http://cpa.test/v0/analytics/viewer/session", remoteAddr: "127.0.0.1:1234", want: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, _, created, _ := newViewerRouteFixture(t, test.options)
			request := httptest.NewRequest(http.MethodPost, test.url, strings.NewReader(`{"credential":"`+created.Credential+`"}`))
			request.RemoteAddr = test.remoteAddr
			if test.host != "" {
				request.Host = test.host
			}
			if test.forwarded != "" {
				request.Header.Set("X-Forwarded-Proto", test.forwarded)
			}
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d, body = %s", recorder.Code, test.want, recorder.Body.String())
			}
			if test.want == http.StatusNoContent {
				cookies := recorder.Result().Cookies()
				if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
					t.Fatalf("session cookies = %#v", cookies)
				}
			}
		})
	}
}

func TestViewerScopeIsServerFixedAndResponseOmitsKeyIdentity(t *testing.T) {
	engine, store, created, reader := newViewerRouteFixture(t, AnalyticsViewerSecurityOptions{})
	token, _, err := store.Exchange(created.Credential)
	if err != nil {
		t.Fatal(err)
	}
	base := "https://cpa.test/v0/analytics/viewer/summary?start=2026-08-01T00:00:00Z&end=2026-09-01T00:00:00Z&time_zone=UTC"
	request := httptest.NewRequest(http.MethodGet, base, nil)
	request.AddCookie(&http.Cookie{Name: analyticsViewerCookieName, Value: token})
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(reader.query.KeyIDs) != 1 || reader.query.KeyIDs[0] != strings.Repeat("a", 64) {
		t.Fatalf("backend key scope = %#v", reader.query.KeyIDs)
	}
	if strings.Contains(recorder.Body.String(), strings.Repeat("a", 64)) || strings.Contains(recorder.Body.String(), "key_id") {
		t.Fatalf("viewer response exposed key identity: %s", recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, base+"&key_ids="+strings.Repeat("b", 64), nil)
	request.AddCookie(&http.Cookie{Name: analyticsViewerCookieName, Value: token})
	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("cross-key status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestViewerResponseAndURLHideLowEntropyKeyIdentity(t *testing.T) {
	engine, store, _, reader := newViewerRouteFixture(t, AnalyticsViewerSecurityOptions{})
	rawKey := "weak-key"
	keyID := config.APIKeyID(rawKey)
	created, err := store.Create(managementHandlers.ViewerCreateRequest{
		KeyID: keyID, AllowedViews: []string{"summary"}, ExpiresAt: time.Now().UTC().Add(time.Hour), Label: "weak key report",
	})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.Exchange(created.Credential)
	if err != nil {
		t.Fatal(err)
	}
	rawURL := "https://cpa.test/v0/analytics/viewer/summary?start=2026-08-01T00:00:00Z&end=2026-09-01T00:00:00Z&time_zone=UTC"
	request := httptest.NewRequest(http.MethodGet, rawURL, nil)
	request.AddCookie(&http.Cookie{Name: analyticsViewerCookieName, Value: token})
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(reader.query.KeyIDs) != 1 || reader.query.KeyIDs[0] != keyID {
		t.Fatalf("status=%d query=%+v body=%s", response.Code, reader.query, response.Body.String())
	}
	for _, forbidden := range []string{rawKey, keyID, keyID[:12]} {
		if strings.Contains(request.URL.String(), forbidden) || strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("viewer URL or response exposed %q", forbidden)
		}
	}
}

func TestViewerEventsStripAdminOnlyIdentityFields(t *testing.T) {
	engine, store, created, reader := newViewerRouteFixture(t, AnalyticsViewerSecurityOptions{})
	credentialID := strings.Repeat("c", 64)
	algorithm := model.CredentialIDAlgorithm
	reader.events = model.EventPage{Events: []model.Event{{
		AttemptID: strings.Repeat("1", 32), ProxyRequestID: strings.Repeat("2", 32), KeyID: strings.Repeat("a", 64),
		RequestedAt: time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), Provider: "provider", Model: "model",
		EndpointClass: "responses", CredentialID: &credentialID, CredentialIDAlgorithm: &algorithm,
	}}}
	token, _, err := store.Exchange(created.Credential)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://cpa.test/v0/analytics/viewer/events?start=2026-08-01T00:00:00Z&end=2026-09-01T00:00:00Z&time_zone=UTC&page_size=10", nil)
	request.AddCookie(&http.Cookie{Name: analyticsViewerCookieName, Value: token})
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	for _, forbidden := range []string{strings.Repeat("a", 64), credentialID, "credential_id", "key_id"} {
		if strings.Contains(recorder.Body.String(), forbidden) {
			t.Fatalf("viewer event response contains %q: %s", forbidden, recorder.Body.String())
		}
	}
}

func TestViewerCORSUsesTrustedProxySchemeWithoutWildcard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	security, err := newAnalyticsViewerSecurity(AnalyticsViewerSecurityOptions{TrustedProxyCIDRs: []string{"203.0.113.0/24"}})
	if err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.Use(corsMiddlewareWithViewerSecurity(security))
	engine.OPTIONS("/v0/analytics/viewer/session", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	request := httptest.NewRequest(http.MethodOptions, "http://cpa.test/v0/analytics/viewer/session", nil)
	request.RemoteAddr = "203.0.113.8:4321"
	request.Header.Set("Origin", "https://cpa.test")
	request.Header.Set("X-Forwarded-Proto", "https")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || recorder.Header().Get("Access-Control-Allow-Origin") != "https://cpa.test" || recorder.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("trusted preflight status=%d headers=%v", recorder.Code, recorder.Header())
	}
	if recorder.Header().Get("Access-Control-Allow-Origin") == "*" {
		t.Fatal("viewer preflight inherited wildcard CORS")
	}

	request = httptest.NewRequest(http.MethodOptions, "http://cpa.test/v0/analytics/viewer/session", nil)
	request.RemoteAddr = "198.51.100.9:4321"
	request.Header.Set("Origin", "https://cpa.test")
	request.Header.Set("X-Forwarded-Proto", "https")
	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || recorder.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("spoofed preflight status=%d headers=%v", recorder.Code, recorder.Header())
	}
}

func TestViewerSessionExchangeRateLimitIsBounded(t *testing.T) {
	engine, _, created, _ := newViewerRouteFixture(t, AnalyticsViewerSecurityOptions{})
	for attempt := 1; attempt <= 11; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "https://cpa.test/v0/analytics/viewer/session", strings.NewReader(`{"credential":"`+created.Credential+`"}`))
		request.RemoteAddr = "198.51.100.3:4444"
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		if attempt <= 10 && recorder.Code != http.StatusNoContent {
			t.Fatalf("attempt %d status=%d body=%s", attempt, recorder.Code, recorder.Body.String())
		}
		if attempt == 11 && (recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") == "") {
			t.Fatalf("throttled status=%d headers=%v", recorder.Code, recorder.Header())
		}
	}
}
