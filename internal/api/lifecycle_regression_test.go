package api

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/outbox"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitLegacyImportReportsFailure(t *testing.T) {
	b := &analyticsBackend{done: make(chan struct{})}
	// A closed completion channel must carry the worker result.
	b.importErr = errors.New("import failed")
	close(b.done)
	if err := b.WaitLegacyImport(context.Background()); err == nil {
		t.Fatal("import failure reported as success")
	}
}
func TestStopTelemetryEmptyOutboxDoesNotConnect(t *testing.T) {
	s := &Server{cfg: &config.Config{}, telemetry: &telemetryRuntime{dir: t.TempDir(), done: make(chan struct{}), stopIngest: make(chan struct{}), cancel: func() {}}}
	close(s.telemetry.done)
	// No configured handle: a fallback Get would panic.
	s.stopTelemetry()
}
func TestShutdownLoggingWaitHonorsContext(t *testing.T) {
	s := &Server{}
	s.requestLoggerWG.Add(1)
	defer s.requestLoggerWG.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { s.waitRequestLoggers(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("logging wait ignored shutdown context")
	}
}

func TestPlainModeShutdownWithHijackedWebSocket(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	engine := gin.New()
	s.engine = engine
	s.requestLogger = logging.NewFileRequestLogger(true, t.TempDir(), "", 0)
	engine.Use(s.requestLoggingMiddleware())
	entered := make(chan struct{})
	release := make(chan struct{})
	engine.GET("/v1/responses", func(c *gin.Context) {
		conn, _, err := c.Writer.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		close(entered)
		<-release
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.server = &http.Server{Handler: engine}
	go s.server.Serve(listener)
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	defer func() { close(release); s.requestLoggerWG.Wait() }()
	_, _ = conn.Write([]byte("GET /v1/responses HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"))
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Stop(ctx) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("plain mode stop hung on hijacked websocket")
	}
}

func TestDefaultCaptureUsesProcessLogDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	if err := os.Mkdir("logs", 0755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.RequestLog = true
	logger := defaultRequestLoggerFactory(cfg, filepath.Join(t.TempDir(), "config.yaml"))
	now := time.Now()
	if err := logger.LogRequest("/v1/messages", "POST", nil, []byte("{}"), 200, nil, []byte("{}"), nil, nil, nil, nil, nil, "directory-test", now, now); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join("logs", "*.log"))
	if err != nil || len(files) != 1 {
		t.Fatalf("capture is not in process log directory: files=%v err=%v", files, err)
	}
}
func TestCanceledShutdownPreservesNonemptyOutbox(t *testing.T) {
	dir := t.TempDir()
	if err := outbox.Write(dir, &record.Request{RequestID: "canceled-drain", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: &config.Config{}, telemetry: &telemetryRuntime{dir: dir, done: make(chan struct{}), stopIngest: make(chan struct{}), cancel: func() {}}}
	close(s.telemetry.done)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.stopTelemetry(ctx)
	files, err := outbox.List(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("outbox lost on shutdown: %v %v", files, err)
	}
}

func TestServerCaptureReusesConfiguredDirectoryBeforeCloning(t *testing.T) {
	first := t.TempDir()
	t.Chdir(first)
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	if err := os.Mkdir("logs", 0755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.RequestLog = true
	if err := logging.ConfigureLogOutput(cfg); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	defer logging.ConfigureLogOutput(&config.Config{})
	s := NewServer(cfg.CloneForRuntime(), nil, nil, filepath.Join(t.TempDir(), "config.yaml"))
	defer s.Stop(context.Background())
	now := time.Now()
	if err := s.requestLogger.LogRequest("/v1/messages", "POST", nil, []byte("{}"), 200, nil, []byte("{}"), nil, nil, nil, nil, nil, "canonical-dir", now, now); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(first, "logs", "*.log"))
	if err != nil || len(files) != 1 {
		t.Fatalf("server clone lost configured directory: %v %v", files, err)
	}
	updated := cfg.CloneForRuntime()
	updated.RuntimeLogDirectory = ""
	updated.LoggingToFile = true
	s.UpdateClients(updated)
	got, err := logging.ConfiguredLogDirectory(s.cfg)
	if err != nil || got != filepath.Join(first, "logs") {
		t.Fatalf("reload lost canonical directory: %q %v", got, err)
	}
}
