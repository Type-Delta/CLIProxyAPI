package clickhouse

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type countedPingConn struct {
	driver.Conn
	calls atomic.Int64
}

func (c *countedPingConn) Ping(ctx context.Context) error { c.calls.Add(1); return ctx.Err() }
func TestR2CachedGetDoesNotPingBorrowedPool(t *testing.T) {
	conn := &countedPingConn{}
	db := &DB{conn: conn}
	h := NewHandle(Config{DSN: "clickhouse://unused:9000"})
	h.db = db
	for range 10 {
		got, err := h.Get(context.Background())
		if err != nil || got != db {
			t.Fatalf("get=%p %v", got, err)
		}
	}
	if got := conn.calls.Load(); got != 0 {
		t.Fatalf("cached Get performed %d serialized pings", got)
	}
}

type slowPingConn struct {
	driver.Conn
	entered, release chan struct{}
}

func (c *slowPingConn) Ping(context.Context) error { close(c.entered); <-c.release; return nil }
func TestR2HealthProbeDoesNotBlockPoolBorrowers(t *testing.T) {
	conn := &slowPingConn{entered: make(chan struct{}), release: make(chan struct{})}
	h := NewHandle(Config{DSN: "clickhouse://unused:9000"})
	h.db = &DB{conn: conn}
	probeDone := make(chan error, 1)
	go func() { probeDone <- h.CheckHealth(context.Background()) }()
	<-conn.entered
	done := make(chan struct{})
	go func() { _, _ = h.Get(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		close(conn.release)
		t.Fatal("health probe holds pool ownership lock")
	}
	// Concurrent health checks use the previous cached health while a probe runs.
	if err := h.CheckHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(conn.release)
	if err := <-probeDone; err != nil {
		t.Fatal(err)
	}
}

func TestR2CanceledHealthProbeDoesNotPoisonHealthyPool(t *testing.T) {
	conn := &countedPingConn{}
	h := NewHandle(Config{DSN: "clickhouse://unused:9000"})
	h.db = &DB{conn: conn}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.CheckHealth(ctx); err == nil {
		t.Fatal("canceled health probe succeeded")
	}
	if err := h.CheckHealth(context.Background()); err != nil {
		t.Fatalf("canceled probe poisoned cache: %v", err)
	}
}
