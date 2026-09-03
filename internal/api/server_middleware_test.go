package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// Tests for applyViewerSameOriginPolicy's analytics.viewer.allowed-origins
// cross-origin allowlist (R4-9 server half): allowed cross-origin requests
// get CORS headers and pass, unknown origins still 403, and same-origin
// behavior is unchanged.

func newAllowedOriginFixture(t *testing.T, allowedOrigins []string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	security, err := newAnalyticsViewerSecurity(AnalyticsViewerSecurityOptions{AllowedOrigins: allowedOrigins})
	if err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.Use(corsMiddlewareWithViewerSecurity(security))
	engine.POST("/v0/analytics/viewer/session", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"crossOrigin": c.GetBool(analyticsViewerCrossOriginContextKey)})
	})
	engine.OPTIONS("/v0/analytics/viewer/session", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	return engine
}

func TestApplyViewerSameOriginPolicyAllowsConfiguredCrossOrigin(t *testing.T) {
	engine := newAllowedOriginFixture(t, []string{"http://127.0.0.1:15173"})

	request := httptest.NewRequest(http.MethodOptions, "http://cpa.test/v0/analytics/viewer/session", nil)
	request.Header.Set("Origin", "http://127.0.0.1:15173")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:15173" {
		t.Fatalf("Access-Control-Allow-Origin = %q", got)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("Access-Control-Allow-Credentials = %q", got)
	}
	if got := recorder.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q", got)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST, OPTIONS" {
		t.Fatalf("Access-Control-Allow-Methods = %q", got)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Headers"); got != "Content-Type" {
		t.Fatalf("Access-Control-Allow-Headers = %q", got)
	}

	request = httptest.NewRequest(http.MethodPost, "http://cpa.test/v0/analytics/viewer/session", nil)
	request.Header.Set("Origin", "http://127.0.0.1:15173")
	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"crossOrigin":true`) {
		t.Fatalf("handler did not observe cross-origin context key: %s", recorder.Body.String())
	}
}

func TestApplyViewerSameOriginPolicyRejectsUnknownOrigin(t *testing.T) {
	engine := newAllowedOriginFixture(t, []string{"http://127.0.0.1:15173"})

	request := httptest.NewRequest(http.MethodOptions, "http://cpa.test/v0/analytics/viewer/session", nil)
	request.Header.Set("Origin", "http://evil.example")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin leaked for unknown origin: %q", got)
	}
}

func TestApplyViewerSameOriginPolicySameOriginUnchanged(t *testing.T) {
	engine := newAllowedOriginFixture(t, []string{"http://127.0.0.1:15173"})

	request := httptest.NewRequest(http.MethodPost, "http://cpa.test/v0/analytics/viewer/session", nil)
	request.Header.Set("Origin", "http://cpa.test")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "http://cpa.test" {
		t.Fatalf("Access-Control-Allow-Origin = %q", got)
	}
	if !strings.Contains(recorder.Body.String(), `"crossOrigin":false`) {
		t.Fatalf("same-origin request was incorrectly flagged cross-origin: %s", recorder.Body.String())
	}
}
