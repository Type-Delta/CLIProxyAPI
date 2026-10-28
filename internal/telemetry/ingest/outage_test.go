package ingest

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/outbox"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/store"
)

type gateProxy struct {
	mu      sync.Mutex
	blocked bool
	conns   map[net.Conn]bool
	target  string
}

func (p *gateProxy) serve(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.blocked {
			p.mu.Unlock()
			_ = c.Close()
			continue
		}
		p.conns[c] = true
		p.mu.Unlock()
		go func() {
			defer func() { p.mu.Lock(); delete(p.conns, c); p.mu.Unlock(); _ = c.Close() }()
			up, e := net.Dial("tcp", p.target)
			if e != nil {
				return
			}
			defer up.Close()
			go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
			_, _ = io.Copy(c, up)
		}()
	}
}
func (p *gateProxy) block(v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blocked = v
	if v {
		for c := range p.conns {
			_ = c.Close()
		}
	}
}
func TestUnavailableOutboxResumes(t *testing.T) {
	ctx := context.Background()
	base := chtest.New(t)
	raw := os.Getenv("CPA_TEST_CLICKHOUSE_DSN")
	if raw == "" {
		b, e := exec.Command("docker", "port", "cpa-test-clickhouse", "9000/tcp").Output()
		if e != nil {
			t.Skip(e)
		}
		raw = "clickhouse://" + strings.TrimSpace(strings.Split(string(b), "\n")[0])
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "clickhouse" && u.Scheme != "tcp" {
		t.Skip("test proxy requires native TCP DSN")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p := &gateProxy{target: u.Host, conns: map[net.Conn]bool{}}
	go p.serve(l)
	u.Host = l.Addr().String()
	h := clickhouse.NewHandle(clickhouse.Config{DSN: u.String(), Database: base.Database()})
	defer h.Close()
	db, err := h.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ing := New(s, dir, 0, 30)
	if err = outbox.Write(dir, &record.Request{RequestID: "outage", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	p.block(true)
	if err = ing.Drain(ctx); !errors.Is(err, ErrPaused) {
		t.Fatalf("expected paused: %v", err)
	}
	paths, err := outbox.List(dir)
	if err != nil || len(paths) != 1 {
		t.Fatalf("outbox %v: %v", paths, err)
	}
	// An unrelated failed borrower does not replace the shared pool.
	if borrowed, errGet := h.Get(ctx); errGet == nil && borrowed != db {
		t.Fatal("shared pool was replaced")
	}
	p.block(false)
	if err = ing.Drain(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	paths, err = outbox.List(dir)
	if err != nil || len(paths) != 0 {
		t.Fatalf("outbox %v: %v", paths, err)
	}
}

func TestIdleDrainDoesNotReportHealthyDuringOutage(t *testing.T) {
	ctx := context.Background()
	base := chtest.New(t)
	raw := os.Getenv("CPA_TEST_CLICKHOUSE_DSN")
	if raw == "" {
		b, e := exec.Command("docker", "port", "cpa-test-clickhouse", "9000/tcp").Output()
		if e != nil {
			t.Skip(e)
		}
		raw = "clickhouse://" + strings.TrimSpace(strings.Split(string(b), "\n")[0])
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "clickhouse" && u.Scheme != "tcp" {
		t.Skip("test proxy requires native TCP DSN")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p := &gateProxy{target: u.Host, conns: map[net.Conn]bool{}}
	go p.serve(l)
	u.Host = l.Addr().String()
	h := clickhouse.NewHandle(clickhouse.Config{DSN: u.String(), Database: base.Database()})
	defer h.Close()
	db, err := h.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ing := New(s, dir, 0, 30)
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	ing.nowFunc = func() time.Time { return now }

	p.block(true)
	if err = ing.Drain(ctx); !errors.Is(err, ErrPaused) {
		t.Fatalf("idle ClickHouse outage reported healthy: %v status=%+v", err, ing.Status())
	}
	if !ing.Status().LastSuccess.IsZero() {
		t.Fatal("idle drain reported successful ingestion")
	}
	p.block(false)
	now = now.Add(29 * time.Second)
	if err = ing.Drain(ctx); !errors.Is(err, ErrPaused) {
		t.Fatalf("idle health was checked too soon: %v", err)
	}
	now = now.Add(2 * time.Second)
	if err = ing.Drain(ctx); err != nil {
		t.Fatalf("idle pool did not recover: %v", err)
	}
	if ing.Status().PausedReason != "" || !ing.Status().LastSuccess.IsZero() {
		t.Fatalf("recovery health inaccurate: %+v", ing.Status())
	}
	// Between health checks, even a nil store must be safe: no ping/disk query.
	ing.store = nil
	now = now.Add(10 * time.Second)
	if err = ing.Drain(ctx); err != nil {
		t.Fatal(err)
	}
}
