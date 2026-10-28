package legacylog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestParseFileLogger(t *testing.T) {
	dir := t.TempDir()
	logger := logging.NewFileRequestLogger(true, dir, dir, 0)
	at := time.Now().UTC()
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	if err := logger.LogRequest("/v1/messages", "POST", map[string][]string{"X-Test": {"value"}}, body, 403, map[string][]string{"Content-Type": {"application/json"}}, []byte(`{"error":"denied"}`), nil, nil, nil, nil, nil, "12345678", at, at); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil || len(files) != 1 {
		t.Fatalf("files %v: %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(filepath.Base(files[0]), data)
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Body) != string(body) || r.Status != 403 || string(r.Response) != `{"error":"denied"}` {
		t.Fatalf("parsed request: %+v, body=%q response=%q", r, r.Body, r.Response)
	}
}

func TestWriterVariants(t *testing.T) {
	t.Run("stream", func(t *testing.T) {
		dir := t.TempDir()
		l := logging.NewFileRequestLogger(true, dir, dir, 0)
		w, err := l.LogStreamingRequest("/v1/messages", "POST", nil, []byte("request"), "12345678")
		if err != nil {
			t.Fatal(err)
		}
		if err = w.WriteStatus(200, map[string][]string{"Content-Type": {"text/event-stream"}}); err != nil {
			t.Fatal(err)
		}
		w.WriteChunkAsync([]byte("data: one\n\n"))
		w.WriteChunkAsync([]byte("data: two\n\n"))
		if err = w.Close(); err != nil {
			t.Fatal(err)
		}
		r := parseOnlyFile(t, dir)
		if !r.Streaming || string(r.Response) != "data: one\n\ndata: two\n\n" {
			t.Fatalf("stream %q %+v", r.Response, r)
		}
	})
	t.Run("websocket", func(t *testing.T) {
		dir := t.TempDir()
		l := logging.NewFileRequestLogger(true, dir, dir, 0)
		at := time.Now()
		err := l.LogRequest("/v1/ws", "GET", nil, nil, 101, nil, nil, []byte("client event"), nil, nil, []byte("upstream event"), nil, "12345678", at, at)
		if err != nil {
			t.Fatal(err)
		}
		r := parseOnlyFile(t, dir)
		if r.Transport != "websocket" || !strings.Contains(string(r.WebsocketTimeline), "client event") || len(r.Attempts) != 1 || !strings.Contains(string(r.Attempts[0].WebsocketTimeline), "upstream event") {
			t.Fatalf("websocket %+v", r)
		}
	})
	t.Run("forced error", func(t *testing.T) {
		dir := t.TempDir()
		l := logging.NewFileRequestLogger(false, dir, dir, 0)
		at := time.Now()
		err := l.LogRequestWithOptions("/v1/messages", "POST", nil, nil, 500, nil, []byte("fail"), nil, nil, nil, nil, []*interfaces.ErrorMessage{{StatusCode: 502, Error: errors.New("bad gateway")}}, true, "12345678", at, at)
		if err != nil {
			t.Fatal(err)
		}
		r := parseOnlyFile(t, dir)
		if r.Detail != "error" || len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "bad gateway") {
			t.Fatalf("error %+v", r)
		}
	})
	t.Run("attempts", func(t *testing.T) {
		dir := t.TempDir()
		gc, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx := context.WithValue(context.Background(), "gin", gc)
		cfg := &config.Config{}
		cfg.RequestLog = true
		for i := 1; i <= 2; i++ {
			helps.RecordAPIRequest(ctx, cfg, helps.UpstreamRequestLog{URL: fmt.Sprintf("https://upstream/%d", i), Method: "POST", Headers: http.Header{"X-Test": {"ok"}}, Body: []byte(fmt.Sprintf("body%d", i))})
			helps.RecordAPIResponseMetadata(ctx, cfg, 400+i, http.Header{"X-Reply": {"yes"}})
			helps.AppendAPIResponseChunk(ctx, cfg, []byte(fmt.Sprintf("response%d", i)))
			if i == 2 {
				helps.RecordAPIResponseError(ctx, cfg, errors.New("broken"))
			}
		}
		req, _ := gc.Get("API_REQUEST")
		resp, _ := gc.Get("API_RESPONSE")
		l := logging.NewFileRequestLogger(true, dir, dir, 0)
		at := time.Now()
		if err := l.LogRequest("/v1/messages", "POST", nil, []byte("hello"), 200, nil, []byte("ok"), nil, req.([]byte), resp.([]byte), nil, nil, "12345678", at, at); err != nil {
			t.Fatal(err)
		}
		r := parseOnlyFile(t, dir)
		if len(r.Attempts) != 2 {
			t.Fatalf("attempts %+v", r.Attempts)
		}
		if len(r.Attempts[1].Errors) == 0 {
			t.Fatalf("missing attempt error: %+v", r.Attempts[1])
		}
		for i, a := range r.Attempts {
			if a.URL != fmt.Sprintf("https://upstream/%d", i+1) || string(a.Body) != fmt.Sprintf("body%d", i+1) || a.Status != 401+i || !strings.Contains(string(a.Response), fmt.Sprintf("response%d", i+1)) {
				t.Fatalf("attempt %+v body %q response %q", a, a.Body, a.Response)
			}
		}
	})
}
func parseOnlyFile(t *testing.T, dir string) *record.Request {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil || len(files) != 1 {
		t.Fatalf("files %v: %v", files, err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(filepath.Base(files[0]), b)
	if err != nil {
		t.Fatalf("parse %v\n%s", err, b)
	}
	return r
}
