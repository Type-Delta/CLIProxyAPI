package api

import (
	"context"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	management "github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// queryGate holds agent provisioning after telemetry has applied its initial TTL.
type queryGate struct {
	io.Reader
	seen             []byte
	once             *sync.Once
	entered, release chan struct{}
}

func (g *queryGate) Read(p []byte) (int, error) {
	n, err := g.Reader.Read(p)
	g.seen = append(g.seen, p[:n]...)
	if strings.Contains(string(g.seen), "CREATE SETTINGS PROFILE") {
		g.once.Do(func() { close(g.entered); <-g.release })
	}
	if len(g.seen) > 4096 {
		g.seen = g.seen[len(g.seen)-4096:]
	}
	return n, err
}
func TestTelemetryRetentionReloadDuringStartup(t *testing.T) {
	root := chtest.Handle(t)
	ctx := context.Background()
	db, err := root.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("CPA_TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		out, e := exec.Command("docker", "port", "cpa-test-clickhouse", "9000/tcp").Output()
		if e != nil {
			t.Fatal(e)
		}
		dsn = "clickhouse://" + strings.TrimSpace(strings.Split(string(out), "\n")[0])
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	target := u.Host
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseGate()
	defer listener.Close()
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				upstream, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer upstream.Close()
				go func() {
					_, _ = io.Copy(upstream, &queryGate{Reader: client, once: &once, entered: entered, release: release})
					_ = upstream.Close()
				}()
				_, _ = io.Copy(client, upstream)
			}()
		}
	}()
	u.Host = listener.Addr().String()
	h := clickhouse.NewHandle(clickhouse.Config{DSN: u.String(), Database: db.Database()})
	defer h.Close()
	cfg := &config.Config{ClickHouse: config.DefaultClickHouseConfig()}
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: management.NewHandler(cfg, "", nil)}
	s.clickHouse.handle = h
	s.startTelemetry(t.TempDir())
	defer func() { releaseGate(); s.stopTelemetry() }()
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("startup never reached agent provisioning")
	}
	s.cfgMu.Lock()
	s.cfg.ClickHouse.TelemetryRetentionDays = 7
	s.cfgMu.Unlock()
	releaseGate()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("retention reload during startup was lost")
		case <-ticker.C:
			var engine string
			if err := db.Conn().QueryRow(ctx, "SELECT engine_full FROM system.tables WHERE database=? AND name='requests'", db.Database()).Scan(&engine); err == nil && strings.Contains(engine, "toIntervalDay(7)") {
				return
			}
		}
	}
}
