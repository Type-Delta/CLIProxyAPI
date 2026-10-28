package api

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestR2PlainShutdownPromptlyClosesOwnedWebSocket(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.RequestLog = true
	assertPromptWebSocketShutdown(t, cfg)
}

func assertPromptWebSocketShutdown(t *testing.T, cfg *config.Config) {
	t.Helper()
	entered := make(chan struct{})
	server := NewServer(cfg, nil, nil, "")
	engine := gin.New()
	engine.Use(logging.GinLogrusLogger(), logging.CPATraceIDMiddleware(), ProxyRequestIDMiddleware(), server.ownedHijacksMiddleware(), server.requestLoggingMiddleware())
	engine.GET("/v1/responses", func(c *gin.Context) {
		ws, err := (&websocket.Upgrader{}).Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		close(entered)
		_, _, _ = ws.ReadMessage()
	})
	server.engine = engine
	if server.telemetry != nil {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		for {
			server.telemetry.mu.RLock()
			ready := server.telemetry.ingester != nil
			server.telemetry.mu.RUnlock()
			if ready {
				break
			}
			select {
			case <-ticker.C:
			case <-deadline.C:
				t.Fatal("telemetry did not become ready")
			}
		}
	}

	httpServer := httptest.NewServer(server.engine)
	defer httpServer.Close()
	server.server = httpServer.Config
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpServer.URL, "http")+"/v1/responses", http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	budget := 300 * time.Millisecond
	if cfg.ClickHouse.DSN != "" {
		budget = 2 * time.Second
	}
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Stop(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(budget):
		_ = client.Close()
		<-done
		t.Fatal("plain shutdown waited on its deadline instead of closing owned websocket")
	}
}

func TestR2ServerShutdownFlushesOwnedWebSocketCapture(t *testing.T) {
	root := chtest.Handle(t)
	db, err := root.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("CPA_TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		out, err := exec.Command("docker", "port", "cpa-test-clickhouse", "9000/tcp").Output()
		if err != nil {
			t.Fatal(err)
		}
		dsn = "clickhouse://" + strings.TrimSpace(strings.Split(string(out), "\n")[0])
	}
	cfg := &config.Config{AuthDir: t.TempDir(), ClickHouse: config.DefaultClickHouseConfig()}
	cfg.ClickHouse.DSN = dsn
	cfg.ClickHouse.Database = db.Database()
	cfg.ClickHouse.AgentQuery.Enabled = false
	cfg.RequestLog = true
	assertPromptWebSocketShutdown(t, cfg)
	var count uint64
	if err := db.Conn().QueryRow(context.Background(), "SELECT count() FROM "+"`"+db.Database()+"`.requests WHERE path='/v1/responses'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("shutdown lost or duplicated websocket capture: rows=%d", count)
	}
}
