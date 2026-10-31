package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCodexCompressionRetryCapturesEveryHTTPAttempt(t *testing.T) {
	for _, mode := range []string{"buffered", "streaming"} {
		t.Run(mode, func(t *testing.T) {
			stream := mode == "streaming"
			requests := 0
			transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				if _, errRead := io.Copy(io.Discard, r.Body); errRead != nil {
					t.Fatal(errRead)
				}
				if requests == 1 {
					if r.Header.Get("Content-Encoding") != "zstd" {
						t.Fatal("expected compressed first request")
					}
					return &http.Response{StatusCode: http.StatusUnsupportedMediaType, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"compression rejected"}}`))}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + codexCompletedEventBody + "\n\n"))}, nil
			})
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			capture := logging.NewTelemetryCapture(t.TempDir())
			c.Set(logging.TelemetryCaptureContextKey, capture)
			ctx := context.WithValue(t.Context(), "gin", c)
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", transport)
			req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"` + strings.Repeat("compressible prompt ", 500) + `"}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: stream}
			e := NewCodexExecutor(codexCompressionConfig())
			if stream {
				res, err := e.ExecuteStream(ctx, codexCompressionAuth(), req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for c := range res.Chunks {
					if c.Err != nil {
						t.Fatal(c.Err)
					}
				}
			} else {
				if _, err := e.Execute(ctx, codexCompressionAuth(), req, opts); err != nil {
					t.Fatal(err)
				}
			}
			attempts := capture.Finish()

			if requests != 2 {
				t.Fatalf("expected two HTTP attempts, got %d", requests)
			}
			if len(attempts) != 2 {
				t.Fatalf("lost compression rejection and plaintext retry identity: HTTP=%d captured=%d", requests, len(attempts))
			}
			for i, want := range []int{http.StatusUnsupportedMediaType, http.StatusOK} {
				if attempts[i].Status != want || attempts[i].Index != i+1 {
					t.Errorf("attempt %d: status=%d index=%d, want status=%d index=%d", i, attempts[i].Status, attempts[i].Index, want, i+1)
				}
			}
			if http.Header(attempts[0].Headers).Get("Content-Encoding") != "zstd" || http.Header(attempts[1].Headers).Get("Content-Encoding") != "" {
				t.Error("capture lost compressed/plaintext attempt identity")
			}
			if !strings.Contains(string(attempts[0].Response), "compression rejected") {
				t.Errorf("rejection body lost: %s", attempts[0].Response)
			}
			if string(attempts[0].Body) != string(attempts[1].Body) {
				t.Error("retry changed the final business payload")
			}
		})
	}
}
