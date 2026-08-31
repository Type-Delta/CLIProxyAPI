package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestProxyRequestIDMiddlewareAssignsOneObservedID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ProxyRequestIDMiddleware())
	router.GET("/v1/test", func(c *gin.Context) {
		fromGin := ProxyRequestIDFromGin(c)
		fromContext := coreusage.ProxyRequestIDFromContext(c.Request.Context())
		if fromGin == "" || fromGin != fromContext || !coreusage.ValidProxyRequestID(fromGin) {
			t.Fatalf("proxy request IDs = gin %q context %q", fromGin, fromContext)
		}
		if endpointClass := coreusage.EndpointClassFromContext(c.Request.Context()); endpointClass != "other" {
			t.Fatalf("endpoint class = %q, want other", endpointClass)
		}
		c.Status(http.StatusNoContent)
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/test", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
}

func TestUsageEndpointClassNeverReturnsRawPath(t *testing.T) {
	tests := map[string]string{
		"/v1/chat/completions":                  "chat_completions",
		"/openai/v1/responses":                  "responses",
		"/v1beta/models/gemini:generateContent": "gemini_generate",
		"/private/customer/123":                 "other",
	}
	for path, want := range tests {
		if got := usageEndpointClass(path); got != want {
			t.Fatalf("usageEndpointClass(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestProxyRequestIDMiddlewarePreservesExistingContextID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	want := "d1371f43e6b8362d05d7567ed5fcc2ad"
	router := gin.New()
	router.Use(ProxyRequestIDMiddleware())
	router.GET("/v1/test", func(c *gin.Context) {
		if got := ProxyRequestIDFromGin(c); got != want {
			t.Fatalf("proxy request ID = %q, want %q", got, want)
		}
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	request = request.WithContext(coreusage.WithProxyRequestID(request.Context(), want))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
}
