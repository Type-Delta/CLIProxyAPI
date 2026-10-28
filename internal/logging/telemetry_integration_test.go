package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/outbox"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
)

func TestTelemetryCaptureRequestAndAttempts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name            string
		enabled, stream bool
		status, count   int
	}{{"full", true, false, 200, 1}, {"stream", true, true, 200, 1}, {"error", false, false, 502, 1}, {"success-error-only", false, false, 200, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logger := logging.NewTelemetryRequestLogger(tc.enabled, dir, "")
			router := gin.New()
			router.Use(func(c *gin.Context) { logging.SetGinRequestID(c, "test-id"); c.Next() })
			router.Use(middleware.RequestLoggingMiddleware(logger))
			router.POST("/v1/chat/completions", func(c *gin.Context) {
				c.Set("userApiKey", "client-secret")
				ctx := context.WithValue(context.Background(), "gin", c)
				cfg := &config.Config{}
				helps.RecordAPIRequest(ctx, cfg, helps.UpstreamRequestLog{URL: "https://example.org/v1?key=upstream-secret", Method: "POST", Headers: http.Header{"Authorization": {"Bearer upstream-secret"}}, Body: []byte("first"), Provider: "one", AuthValue: "upstream-secret"})
				helps.RecordAPIResponseError(ctx, cfg, errors.New("connection failed for upstream-secret"))
				helps.RecordAPIRequest(ctx, cfg, helps.UpstreamRequestLog{URL: "https://example.org/v1", Method: "POST", Body: []byte("second"), Provider: "two"})
				helps.RecordAPIResponseMetadata(ctx, cfg, 200, http.Header{"Content-Type": {"text/event-stream"}})
				helps.AppendAPIResponseChunk(ctx, cfg, []byte("data: x\n\n"))
				if tc.stream {
					c.Header("Content-Type", "text/event-stream")
					c.Status(tc.status)
					_, _ = c.Writer.Write([]byte("data: result\n\n"))
				} else {
					c.String(tc.status, "response")
				}
			})
			request := httptest.NewRequest("POST", "/v1/chat/completions?key=client-secret", strings.NewReader(`{"messages":[]}`))
			request.Header.Set("Authorization", "Bearer client-secret")
			request.Header.Set("Cookie", "session=cookie-secret")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			paths, err := outbox.List(dir)
			if err != nil || len(paths) != tc.count {
				t.Fatalf("paths %v, err %v", paths, err)
			}
			if tc.count == 0 {
				return
			}
			got, err := outbox.Read(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			if got.ActorKeyID != config.APIKeyID("client-secret") || len(got.Attempts) != 2 || string(got.Attempts[1].Response) != "data: x\n\n" || len(got.Attempts[0].Errors) != 1 {
				t.Fatalf("record: %+v", got)
			}
			if tc.stream && (!got.Streaming || string(got.Response) != "data: result\n\n" || got.FirstChunkAt.IsZero()) {
				t.Fatalf("stream record: %+v", got)
			}
			encoded, _ := json.Marshal(got)
			if strings.Contains(string(encoded), "client-secret") || strings.Contains(string(encoded), "upstream-secret") || strings.Contains(string(encoded), "cookie-secret") {
				t.Fatalf("secret in record: %s", encoded)
			}
			if !tc.enabled && got.Detail != "error" {
				t.Fatalf("detail %q", got.Detail)
			}
		})
	}
}

func TestTelemetryRenderMatchesFileLoggerWithoutAttempts(t *testing.T) {
	dir := t.TempDir()
	started := time.Unix(1700000000, 0).UTC()
	logger := logging.NewFileRequestLogger(true, dir, "", 0)
	err := logger.LogRequest("/v1/chat/completions", "POST", map[string][]string{"Content-Type": {"application/json"}}, []byte(`{"x":1}`), 200, map[string][]string{"Content-Type": {"application/json"}}, []byte(`{"ok":true}`), nil, nil, nil, nil, nil, "render-test", started, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("files=%v err=%v", entries, err)
	}
	file, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	rendered := logging.RenderRequestLog(&record.Request{StartedAt: started, URL: "/v1/chat/completions", Method: "POST", Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"x":1}`), Status: 200, ResponseHeaders: map[string][]string{"Content-Type": {"application/json"}}, Response: []byte(`{"ok":true}`)})
	if !bytes.Equal(file, rendered) {
		t.Fatalf("file:\n%s\nrendered:\n%s", file, rendered)
	}
}

func TestTelemetryWebsocketAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	capture := logging.NewTelemetryCapture(t.TempDir())
	c.Set(logging.TelemetryCaptureContextKey, capture)
	ctx := context.WithValue(context.Background(), "gin", c)
	info := helps.UpstreamRequestLog{URL: "wss://example.org/socket?key=secret", Method: "WEBSOCKET", Headers: http.Header{"Authorization": {"Bearer secret"}}, Body: []byte("hello"), AuthValue: "secret"}
	helps.RecordAPIWebsocketRequest(ctx, nil, info)
	helps.RecordAPIWebsocketHandshake(ctx, nil, 101, http.Header{})
	helps.AppendAPIWebsocketResponse(ctx, nil, []byte("frame"))
	helps.RecordAPIWebsocketError(ctx, nil, "receive", errors.New("closed"))
	attempts := capture.Finish()
	if len(attempts) != 1 || attempts[0].Transport != "websocket" || attempts[0].Status != 101 || string(attempts[0].Response) != "frame" || !bytes.Contains(attempts[0].WebsocketTimeline, []byte(`"event":"response"`)) || len(attempts[0].Errors) != 1 {
		t.Fatalf("attempts: %+v", attempts)
	}
	data, _ := json.Marshal(attempts)
	if bytes.Contains(data, []byte("secret")) {
		t.Fatalf("secret in attempts: %s", data)
	}
}

func TestTelemetryCommercialModeSuppressesUpstreamCapture(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	capture := logging.NewTelemetryCapture(t.TempDir())
	c.Set(logging.TelemetryCaptureContextKey, capture)
	ctx := context.WithValue(context.Background(), "gin", c)
	cfg := &config.Config{CommercialMode: true}
	cfg.RequestLog = true
	info := helps.UpstreamRequestLog{URL: "https://example.test", Body: []byte("private upstream")}
	helps.RecordAPIRequest(ctx, cfg, info)
	helps.RecordAPIResponseMetadata(ctx, cfg, 200, nil)
	helps.AppendAPIResponseChunk(ctx, cfg, []byte("private response"))
	helps.RecordAPIResponseError(ctx, cfg, errors.New("private error"))
	helps.RecordAPIWebsocketRequest(ctx, cfg, info)
	helps.RecordAPIWebsocketHandshake(ctx, cfg, 101, nil)
	helps.AppendAPIWebsocketResponse(ctx, cfg, []byte("private frame"))
	helps.RecordAPIWebsocketError(ctx, cfg, "read", errors.New("private error"))
	helps.RecordAPIWebsocketUpgradeRejection(ctx, cfg, info, 403, nil, []byte("private rejection"))
	if got := capture.Finish(); len(got) != 0 {
		t.Fatalf("commercial mode captured %d attempts", len(got))
	}
}
