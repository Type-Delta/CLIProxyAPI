package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	log "github.com/sirupsen/logrus"
)

const analyticsViewerCookieName = "cpa_analytics_viewer"

type AnalyticsViewerSecurityOptions struct {
	TrustedProxyCIDRs []string
	AllowLoopbackHTTP bool
}

type analyticsViewerSecurity struct {
	trustedProxies    []netip.Prefix
	allowLoopbackHTTP bool
}

// RegisterAnalyticsViewerRoutes installs the separately authenticated viewer
// group. It must run after the global same-origin viewer CORS branch is active.
func (s *Server) RegisterAnalyticsViewerRoutes(store *managementHandlers.AnalyticsViewerStore, options AnalyticsViewerSecurityOptions) error {
	security, err := newAnalyticsViewerSecurity(options)
	if err != nil {
		return err
	}
	limiter := newViewerRateLimiter(4096)
	group := s.engine.Group("/v0/analytics/viewer")
	group.Use(analyticsRecoveryMiddleware())
	group.POST("/session", func(c *gin.Context) {
		s.exchangeAnalyticsViewerSession(c, store, security, limiter)
	})
	group.GET("/capabilities", func(c *gin.Context) {
		s.getAnalyticsViewerCapabilities(c, store, limiter)
	})
	group.GET("/summary", func(c *gin.Context) {
		s.getAnalyticsViewerSummary(c, store, limiter)
	})
	group.GET("/timeseries", func(c *gin.Context) {
		s.getAnalyticsViewerTimeseries(c, store, limiter)
	})
	group.GET("/events", func(c *gin.Context) {
		s.getAnalyticsViewerEvents(c, store, limiter)
	})
	return nil
}

func analyticsRecoveryMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recover() == nil {
				return
			}
			logging.SkipGinRequestLogging(c)
			log.WithField("path", c.Request.URL.Path).Error("analytics handler panic recovered")
			managementHandlers.WriteAnalyticsAPIError(c, cpauk.ErrInternal)
		}()
		c.Next()
	}
}

func newAnalyticsViewerSecurity(options AnalyticsViewerSecurityOptions) (analyticsViewerSecurity, error) {
	security := analyticsViewerSecurity{allowLoopbackHTTP: options.AllowLoopbackHTTP}
	for _, raw := range options.TrustedProxyCIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return analyticsViewerSecurity{}, err
		}
		security.trustedProxies = append(security.trustedProxies, prefix.Masked())
	}
	return security, nil
}

func (s *Server) exchangeAnalyticsViewerSession(c *gin.Context, store *managementHandlers.AnalyticsViewerStore, security analyticsViewerSecurity, limiter *viewerRateLimiter) {
	if store == nil || !security.requestAllowsSession(c.Request) {
		managementHandlers.WriteAnalyticsAPIError(c, managementHandlers.ErrViewerCredentialInvalid)
		return
	}
	clientKey := "ip:" + viewerImmediatePeer(c.Request)
	if !limiter.allow(clientKey, 10, time.Minute) {
		c.Header("Retry-After", "60")
		managementHandlers.WriteAnalyticsThrottled(c)
		return
	}
	var request struct {
		Credential string `json:"credential"`
	}
	if err := decodeViewerJSON(c, &request, 1024); err != nil || request.Credential == "" {
		managementHandlers.WriteAnalyticsAPIError(c, managementHandlers.ErrViewerCredentialInvalid)
		return
	}
	credentialDigest := sha256.Sum256([]byte("viewer-rate-v1\x00" + request.Credential))
	if !limiter.allow("credential:"+hex.EncodeToString(credentialDigest[:]), 10, time.Minute) {
		c.Header("Retry-After", "60")
		managementHandlers.WriteAnalyticsThrottled(c)
		return
	}
	token, scope, err := store.Exchange(request.Credential)
	request.Credential = ""
	if err != nil {
		if errors.Is(err, managementHandlers.ErrViewerCredentialInvalid) {
			managementHandlers.WriteAnalyticsAPIError(c, managementHandlers.ErrViewerCredentialInvalid)
		} else {
			managementHandlers.WriteAnalyticsAPIError(c, err)
		}
		return
	}
	maxAge := int(time.Until(scope.ExpiresAt).Seconds())
	if maxAge < 1 {
		managementHandlers.WriteAnalyticsAPIError(c, managementHandlers.ErrViewerCredentialInvalid)
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name: analyticsViewerCookieName, Value: token, Path: "/v0/analytics/viewer",
		Expires: scope.ExpiresAt, MaxAge: maxAge, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	managementHandlers.SetAnalyticsNoStore(c)
	c.Status(http.StatusNoContent)
}

func (s *Server) getAnalyticsViewerCapabilities(c *gin.Context, store *managementHandlers.AnalyticsViewerStore, limiter *viewerRateLimiter) {
	scope, ok := authenticateAnalyticsViewer(c, store, limiter, "")
	if !ok {
		return
	}
	if err := analyticsViewerServiceError(s); err != nil {
		managementHandlers.WriteAnalyticsAPIError(c, err)
		return
	}
	managementHandlers.SetAnalyticsNoStore(c)
	c.JSON(http.StatusOK, model.ViewerCapabilities{
		APISchemaVersion: model.APISchemaVersion, AllowedViews: scope.AllowedViews,
		Label: scope.Label, ExpiresAt: scope.ExpiresAt,
	})
}

func (s *Server) getAnalyticsViewerSummary(c *gin.Context, store *managementHandlers.AnalyticsViewerStore, limiter *viewerRateLimiter) {
	scope, ok := authenticateAnalyticsViewer(c, store, limiter, "summary")
	if !ok {
		return
	}
	if err := analyticsViewerServiceError(s); err != nil {
		managementHandlers.WriteAnalyticsAPIError(c, err)
		return
	}
	query, err := managementHandlers.ParseViewerAnalyticsQuery(c.Request.URL.Query(), model.OperationSummary, scope.KeyID)
	if err != nil {
		managementHandlers.WriteAnalyticsInvalidQuery(c)
		return
	}
	result, err := s.analytics.Reader().Summary(c.Request.Context(), query)
	if err != nil {
		managementHandlers.WriteAnalyticsAPIError(c, classifyViewerAnalyticsError(err))
		return
	}
	managementHandlers.SetAnalyticsNoStore(c)
	c.JSON(http.StatusOK, model.ViewerSummary{
		Meta: result.Meta, Label: scope.Label, ProxyRequests: result.ProxyRequests,
		UpstreamAttempts: result.UpstreamAttempts, Tokens: result.Tokens,
		KnownCost: result.KnownCost, UnpricedTokens: result.UnpricedTokens,
	})
}

func (s *Server) getAnalyticsViewerTimeseries(c *gin.Context, store *managementHandlers.AnalyticsViewerStore, limiter *viewerRateLimiter) {
	scope, ok := authenticateAnalyticsViewer(c, store, limiter, "timeseries")
	if !ok {
		return
	}
	if err := analyticsViewerServiceError(s); err != nil {
		managementHandlers.WriteAnalyticsAPIError(c, err)
		return
	}
	query, err := managementHandlers.ParseViewerAnalyticsQuery(c.Request.URL.Query(), model.OperationTimeseries, scope.KeyID)
	if err != nil {
		managementHandlers.WriteAnalyticsInvalidQuery(c)
		return
	}
	result, err := s.analytics.Reader().Timeseries(c.Request.Context(), query)
	if err != nil {
		managementHandlers.WriteAnalyticsAPIError(c, classifyViewerAnalyticsError(err))
		return
	}
	managementHandlers.SetAnalyticsNoStore(c)
	c.JSON(http.StatusOK, struct {
		Meta   model.ResponseMeta      `json:"meta"`
		Label  string                  `json:"label,omitempty"`
		Points []model.TimeseriesPoint `json:"points"`
	}{Meta: result.Meta, Label: scope.Label, Points: result.Points})
}

func (s *Server) getAnalyticsViewerEvents(c *gin.Context, store *managementHandlers.AnalyticsViewerStore, limiter *viewerRateLimiter) {
	scope, ok := authenticateAnalyticsViewer(c, store, limiter, "events")
	if !ok {
		return
	}
	if err := analyticsViewerServiceError(s); err != nil {
		managementHandlers.WriteAnalyticsAPIError(c, err)
		return
	}
	query, err := managementHandlers.ParseViewerAnalyticsQuery(c.Request.URL.Query(), model.OperationEvents, scope.KeyID)
	if err != nil {
		managementHandlers.WriteAnalyticsInvalidQuery(c)
		return
	}
	result, err := s.analytics.Reader().Events(c.Request.Context(), query)
	if err != nil {
		managementHandlers.WriteAnalyticsAPIError(c, classifyViewerAnalyticsError(err))
		return
	}
	events := make([]model.ViewerEvent, 0, len(result.Events))
	for _, event := range result.Events {
		events = append(events, model.ViewerEvent{
			AttemptID: event.AttemptID, ProxyRequestID: event.ProxyRequestID, RequestedAt: event.RequestedAt,
			Provider: event.Provider, Model: event.Model, EndpointClass: event.EndpointClass,
			Succeeded: event.Succeeded, UpstreamStatusCode: event.UpstreamStatusCode, ErrorClass: event.ErrorClass,
			LatencyMS: event.LatencyMS, Tokens: event.Tokens, KnownCost: event.KnownCost,
			UnpricedTokens: event.UnpricedTokens,
		})
	}
	managementHandlers.SetAnalyticsNoStore(c)
	c.JSON(http.StatusOK, model.ViewerEventPage{Meta: result.Meta, Label: scope.Label, Events: events})
}

func authenticateAnalyticsViewer(c *gin.Context, store *managementHandlers.AnalyticsViewerStore, limiter *viewerRateLimiter, view string) (managementHandlers.ViewerSessionScope, bool) {
	if store == nil {
		managementHandlers.WriteAnalyticsAPIError(c, managementHandlers.ErrViewerSessionInvalid)
		return managementHandlers.ViewerSessionScope{}, false
	}
	cookie, err := c.Request.Cookie(analyticsViewerCookieName)
	if err != nil || cookie.Value == "" {
		managementHandlers.WriteAnalyticsAPIError(c, managementHandlers.ErrViewerSessionInvalid)
		return managementHandlers.ViewerSessionScope{}, false
	}
	if !limiter.allow("viewer-peer:"+viewerImmediatePeer(c.Request), 240, time.Minute) {
		c.Header("Retry-After", "60")
		managementHandlers.WriteAnalyticsThrottled(c)
		return managementHandlers.ViewerSessionScope{}, false
	}
	scope, err := store.Authenticate(cookie.Value, view)
	if err != nil {
		managementHandlers.WriteAnalyticsAPIError(c, err)
		return managementHandlers.ViewerSessionScope{}, false
	}
	if !limiter.allow("session:"+scope.AuditID, 120, time.Minute) {
		c.Header("Retry-After", "60")
		managementHandlers.WriteAnalyticsThrottled(c)
		return managementHandlers.ViewerSessionScope{}, false
	}
	return scope, true
}

func classifyViewerAnalyticsError(err error) error {
	if errors.Is(err, cpauk.ErrDisabled) || errors.Is(err, cpauk.ErrUnavailable) || errors.Is(err, cpauk.ErrClosed) || errors.Is(err, cpauk.ErrInternal) || errors.Is(err, cpauk.ErrMaintenance) {
		return err
	}
	return cpauk.ErrInternal
}

func analyticsViewerServiceError(s *Server) error {
	if s == nil || s.analytics == nil {
		return cpauk.ErrUnavailable
	}
	capabilities := s.analytics.Capabilities()
	if !capabilities.Enabled || capabilities.State == model.StateDisabled {
		return cpauk.ErrDisabled
	}
	return nil
}

func decodeViewerJSON(c *gin.Context, target any, limit int64) error {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return fmt.Errorf("request body is required")
	}
	data, err := io.ReadAll(io.LimitReader(c.Request.Body, limit+1))
	if err != nil || len(data) == 0 || int64(len(data)) > limit {
		return fmt.Errorf("request body exceeds its bounds")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("request must contain one JSON value")
	}
	return nil
}

func (security analyticsViewerSecurity) requestAllowsSession(request *http.Request) bool {
	if request == nil {
		return false
	}
	if request.TLS != nil {
		return true
	}
	peer := netip.MustParseAddr(viewerImmediatePeer(request))
	if slices.ContainsFunc(security.trustedProxies, func(prefix netip.Prefix) bool { return prefix.Contains(peer) }) {
		values := request.Header.Values("X-Forwarded-Proto")
		return len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), "https") && !strings.Contains(values[0], ",")
	}
	return security.allowLoopbackHTTP && peer.IsLoopback() && viewerHostIsLoopback(request.Host)
}

func (security analyticsViewerSecurity) requestScheme(request *http.Request) string {
	if request == nil {
		return "http"
	}
	if request.TLS != nil {
		return "https"
	}
	peer, err := netip.ParseAddr(viewerImmediatePeer(request))
	if err == nil && slices.ContainsFunc(security.trustedProxies, func(prefix netip.Prefix) bool { return prefix.Contains(peer) }) {
		values := request.Header.Values("X-Forwarded-Proto")
		if len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), "https") && !strings.Contains(values[0], ",") {
			return "https"
		}
	}
	return "http"
}

func viewerImmediatePeer(request *http.Request) string {
	if request == nil {
		return "0.0.0.0"
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(request.RemoteAddr))
	if err != nil {
		host = strings.Trim(strings.TrimSpace(request.RemoteAddr), "[]")
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return "0.0.0.0"
	}
	return address.Unmap().String()
}

func viewerHostIsLoopback(hostport string) bool {
	host := hostport
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsed
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

type viewerRateLimiter struct {
	mu         sync.Mutex
	entries    map[string]viewerRateEntry
	maxEntries int
	now        func() time.Time
}

type viewerRateEntry struct {
	windowStart time.Time
	count       int
}

func newViewerRateLimiter(maxEntries int) *viewerRateLimiter {
	return &viewerRateLimiter{entries: make(map[string]viewerRateEntry), maxEntries: maxEntries, now: time.Now}
}

func (l *viewerRateLimiter) allow(key string, limit int, window time.Duration) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, exists := l.entries[key]
	if exists && now.Sub(entry.windowStart) >= window {
		delete(l.entries, key)
		exists = false
	}
	if !exists && len(l.entries) >= l.maxEntries {
		for candidate, value := range l.entries {
			if now.Sub(value.windowStart) >= window {
				delete(l.entries, candidate)
			}
		}
		if len(l.entries) >= l.maxEntries {
			return false
		}
	}
	if !exists {
		entry = viewerRateEntry{windowStart: now}
	}
	if entry.count >= limit {
		return false
	}
	entry.count++
	l.entries[key] = entry
	return true
}
