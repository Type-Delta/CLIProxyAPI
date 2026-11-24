package logging_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/outbox"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestTelemetryRetainsHTTPHeadersWithoutSuccessfulBodies(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streaming"}[stream], func(t *testing.T) {
			dir := t.TempDir()
			router := gin.New()
			router.Use(logging.GinLogrusLogger(), middleware.RequestLoggingMiddleware(logging.NewTelemetryRequestLogger(false, dir, "")))
			router.POST("/v1/chat/completions", func(c *gin.Context) {
				c.Request = c.Request.WithContext(usage.WithProxyRequestID(c.Request.Context(), strings.Repeat("a", 32)))
				middleware.CaptureAuthenticatedRequestBody(c)
				ctx := context.WithValue(c.Request.Context(), "gin", c)
				cfg := &config.Config{}
				helps.RecordAPIRequest(ctx, cfg, helps.UpstreamRequestLog{Method: "POST", URL: "https://upstream.test/v1", Headers: http.Header{"X-Upstream": {"one", "two"}, "Authorization": {"Bearer upstream-secret"}}, Body: []byte("private upstream request")})
				helps.RecordAPIResponseMetadata(ctx, cfg, 200, http.Header{"X-Upstream-Reply": {"yes"}, "Set-Cookie": {"private cookie"}})
				helps.AppendAPIResponseChunk(ctx, cfg, []byte("private upstream response"))
				helps.RecordAPIResponseError(ctx, cfg, errors.New("private upstream error"))
				c.Header("X-Proxy-Reply", "yes")
				if stream {
					c.Header("Content-Type", "text/event-stream")
					_, _ = c.Writer.WriteString("data: private downstream response\n\n")
					c.Writer.Flush()
				} else {
					c.String(200, "private downstream response")
				}
			})
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("private client request"))
			req.Header.Set("Authorization", "Bearer client-secret")
			req.Header.Add("X-Client", "one")
			req.Header.Add("X-Client", "two")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			paths, err := outbox.List(dir)
			if err != nil || len(paths) != 1 {
				t.Fatalf("outbox: %v %v", paths, err)
			}
			got, err := outbox.Read(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			if got.Detail != record.DetailHeaders || len(got.Body) != 0 || len(got.Response) != 0 || len(got.WebsocketTimeline) != 0 || len(got.Attempts) != 1 {
				t.Fatalf("headers-only request retained payloads: %+v", got)
			}
			a := got.Attempts[0]
			if len(a.Body) != 0 || len(a.Response) != 0 || len(a.WebsocketTimeline) != 0 || len(a.Errors) != 0 || len(got.Errors) != 0 || len(a.Headers["X-Upstream"]) != 2 || a.ResponseHeaders["X-Upstream-Reply"][0] != "yes" || len(got.Headers["X-Client"]) != 2 || got.ResponseHeaders["X-Proxy-Reply"][0] != "yes" {
				t.Fatalf("incomplete headers-only capture: %+v", got)
			}
			if strings.Contains(got.Headers["Authorization"][0], "client-secret") || strings.Contains(a.Headers["Authorization"][0], "upstream-secret") || a.ResponseHeaders["Set-Cookie"][0] != "[REDACTED]" {
				t.Fatal("credentials were not masked")
			}
		})
	}
}
