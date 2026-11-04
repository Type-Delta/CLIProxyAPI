package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Kiro attempts must reach the telemetry attempts table like every other
// provider, including upstream rejections and decoded stream events.
func TestKiroExecutorCapturesUpstreamAttempts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
		status int
		want   string
	}{
		{name: "buffered", status: http.StatusOK, want: "event: assistantResponseEvent\ndata: {\"content\":\"hello from Kiro\"}"},
		{name: "streaming", stream: true, status: http.StatusOK, want: "event: assistantResponseEvent\ndata: {\"content\":\"hello from Kiro\"}"},
		{name: "rejected", stream: true, status: http.StatusBadRequest, want: "Invalid tool use format."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status != http.StatusOK {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"message":"Invalid tool use format.","reason":"REQUEST_BODY_INVALID"}`))
					return
				}
				writeKiroTestEvents(t, w, []kiroTestEvent{{name: "assistantResponseEvent", payload: map[string]any{"content": "hello from Kiro"}}})
			}))
			t.Cleanup(server.Close)

			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			capture := logging.NewTelemetryCapture(t.TempDir())
			c.Set(logging.TelemetryCaptureContextKey, capture)
			ctx := context.WithValue(kiroUsageTestContext(server, t.Name()), "gin", c)
			auth := kiroUsageTestAuth()
			auth.Label = "Kiro - Test"
			req := cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: tc.stream}

			executor := NewKiroExecutor(&config.Config{})
			var errRun error
			if tc.stream {
				result, errStream := executor.ExecuteStream(ctx, auth, req, opts)
				errRun = errStream
				if result != nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							errRun = chunk.Err
						}
					}
				}
			} else {
				_, errRun = executor.Execute(ctx, auth, req, opts)
			}
			if (errRun != nil) != (tc.status != http.StatusOK) {
				t.Fatalf("unexpected execution error: %v", errRun)
			}

			attempts := capture.Finish()
			if len(attempts) != 1 {
				t.Fatalf("captured %d Kiro attempts, want 1", len(attempts))
			}
			attempt := attempts[0]
			if attempt.Provider != "kiro" || attempt.AuthID != "kiro-test-auth" || attempt.AuthLabel != "Kiro - Test" || attempt.Status != tc.status {
				t.Fatalf("attempt identity = provider %q auth %q label %q status %d", attempt.Provider, attempt.AuthID, attempt.AuthLabel, attempt.Status)
			}
			if authorization := http.Header(attempt.Headers).Get("Authorization"); authorization == "" || strings.Contains(authorization, "test-token") {
				t.Fatalf("attempt Authorization header is missing or unmasked: %q", authorization)
			}
			if !strings.Contains(string(attempt.Body), "conversationState") {
				t.Fatalf("attempt body is not the Kiro payload: %s", attempt.Body)
			}
			if !strings.Contains(string(attempt.Response), tc.want) {
				t.Fatalf("attempt response = %q, want it to contain %q", attempt.Response, tc.want)
			}
		})
	}
}
