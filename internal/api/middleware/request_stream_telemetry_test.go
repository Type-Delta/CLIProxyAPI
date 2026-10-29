package middleware

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/ingest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/legacylog"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/outbox"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/store"
)

func TestCapturedRequestStreamSurvivesJSON403(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	st, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"telemetry", "legacy file import"} {
		for _, tc := range []struct {
			name, body, contentType, response string
			status                            int
			want, framing                     bool
		}{
			{"failed streaming", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "application/json", `{"error":"forbidden"}`, 403, true, false},
			{"non streaming", `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`, "application/json", `{"content":[]}`, 200, false, false},
			{"successful streaming", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "text/event-stream", "data: {\"type\":\"message_stop\"}\n\n", 200, true, true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				var logger logging.RequestLogger
				if mode == "telemetry" {
					logger = logging.NewTelemetryRequestLogger(tc.status < 400, dir, "")
				} else {
					logger = logging.NewFileRequestLogger(tc.status < 400, dir, "", 0)
				}
				router := gin.New()
				router.Use(logging.GinLogrusLogger(), RequestLoggingMiddleware(logger))
				router.POST("/v1/messages", func(c *gin.Context) {
					if _, err := io.Copy(io.Discard, c.Request.Body); err != nil {
						t.Error(err)
					}
					c.Header("Content-Type", tc.contentType)
					c.Writer.WriteHeader(tc.status)
					if _, err := c.Writer.Write([]byte(tc.response)); err != nil {
						t.Error(err)
					}
				})
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				if w.Code != tc.status || w.Body.String() != tc.response {
					t.Fatalf("response changed: %d %q", w.Code, w.Body.String())
				}
				var captured *record.Request
				ing := ingest.New(st, dir, 0, 30)
				if mode == "telemetry" {
					paths, err := outbox.List(dir)
					if err != nil || len(paths) != 1 {
						t.Fatalf("outbox=%v: %v", paths, err)
					}
					captured, err = outbox.Read(paths[0])
					if err != nil {
						t.Fatal(err)
					}
					if err := ing.Drain(ctx); err != nil {
						t.Fatal(err)
					}
				} else {
					paths, err := filepath.Glob(filepath.Join(dir, "*.log"))
					if err != nil || len(paths) != 1 {
						t.Fatalf("file logs=%v: %v", paths, err)
					}
					data, err := os.ReadFile(paths[0])
					if err != nil {
						t.Fatal(err)
					}
					captured, err = legacylog.Parse(filepath.Base(paths[0]), data)
					if err != nil {
						t.Fatal(err)
					}
					if err := ing.Process(ctx, captured); err != nil {
						t.Fatal(err)
					}
				}
				// The response framing contract, including ordinary file logs, must not change.
				if captured.Streaming != tc.framing || string(captured.Body) != tc.body {
					t.Fatalf("capture framing=%v body=%s", captured.Streaming, captured.Body)
				}
				var stream bool
				if err := db.Conn().QueryRow(ctx, "SELECT stream FROM `"+db.Database()+"`.requests FINAL WHERE request_id = ?", captured.RequestID).Scan(&stream); err != nil {
					t.Fatal(err)
				}
				if stream != tc.want {
					t.Fatalf("stored stream=%v, want %v", stream, tc.want)
				}
			})
		}
	}
}
