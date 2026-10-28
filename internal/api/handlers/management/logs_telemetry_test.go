package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/ingest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/store"
)

func TestClickHouseRequestLogs(t *testing.T) {
	ctx := context.Background()
	h := chtest.Handle(t)
	db, err := h.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"messages":[{"role":"user","content":"hello"}]}`
	ing := ingest.New(s, t.TempDir(), 0, 30)
	if err = ing.IngestRecords(ctx, []*record.Request{{RequestID: "full-12345678", StartedAt: time.Now().UTC(), URL: "/v1/messages", Body: []byte(body), Detail: "error", Status: 403}}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(&config.Config{}, "", nil)
	handler.SetClickHouse(h)
	router := gin.New()
	router.GET("/errors", handler.GetRequestErrorLogs)
	router.GET("/errors/:name", handler.DownloadRequestErrorLog)
	router.GET("/requests/:id", handler.GetRequestLogByID)
	for _, tc := range []struct{ path, want string }{{"/errors", "error-full-12345678.log"}, {"/errors/error-full-12345678.log", body}, {"/requests/12345678", body}, {"/requests/full-12345678", body}} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("%s status=%d body=%s", tc.path, w.Code, w.Body.String())
		}
	}
}

func TestCapturedRequestCanBeDownloaded(t *testing.T) {
	ctx := context.Background()
	handle := chtest.Handle(t)
	db, err := handle.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logger := logging.NewTelemetryRequestLogger(true, dir, "")
	router := gin.New()
	router.Use(func(c *gin.Context) { logging.SetGinRequestID(c, "captured-12345678"); c.Next() })
	router.Use(middleware.RequestLoggingMiddleware(logger))
	router.POST("/v1/messages", func(c *gin.Context) { c.JSON(403, gin.H{"error": "forbidden"}) })
	body := `{"messages":[{"role":"user","content":"live capture"}]}`
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if w.Code != 403 {
		t.Fatalf("capture: %d", w.Code)
	}
	ing := ingest.New(s, dir, 0, 30)
	if err = ing.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{}, "", nil)
	h.SetClickHouse(handle)
	management := gin.New()
	management.GET("/requests/:id", h.GetRequestLogByID)
	w = httptest.NewRecorder()
	management.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/requests/12345678", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), body) || !strings.Contains(w.Body.String(), "forbidden") {
		t.Fatalf("download status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestErrorLogListDoesNotRebuildTranscripts(t *testing.T) {
	ctx := context.Background()
	handle := chtest.Handle(t)
	db, err := handle.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := ingest.New(s, t.TempDir(), 0, 30)
	if err = ing.Process(ctx, &record.Request{RequestID: "list-error", StartedAt: time.Now(), URL: "/v1/messages", Body: []byte(`{"messages":[{"role":"user","content":"body"}]}`), Detail: "error", Status: 403}); err != nil {
		t.Fatal(err)
	}
	// Listing metadata must remain available even when a transcript cannot be reconstructed.
	if err = db.Conn().Exec(ctx, "TRUNCATE TABLE `"+db.Database()+"`.transcript_content"); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{}, "", nil)
	h.SetClickHouse(handle)
	router := gin.New()
	router.GET("/errors", h.GetRequestErrorLogs)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/errors", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "error-list-error.log") {
		t.Fatalf("list rebuilt transcript: %d %s", w.Code, w.Body.String())
	}
}
