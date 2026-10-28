package clickhouse

import (
	"context"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"sync"
)

// agentConnection keeps a retired pool alive until its query rows are released.
type agentConnection struct {
	driver.Conn
	mu sync.RWMutex
}

func (c *agentConnection) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	c.mu.RLock()
	rows, err := c.Conn.Query(ctx, query, args...)
	if err != nil {
		c.mu.RUnlock()
		return nil, err
	}
	return &agentRows{Rows: rows, release: c.mu.RUnlock}, nil
}
func (c *agentConnection) QueryRow(ctx context.Context, query string, args ...any) driver.Row {
	c.mu.RLock()
	return &agentRow{Row: c.Conn.QueryRow(ctx, query, args...), release: c.mu.RUnlock}
}
func (c *agentConnection) Exec(ctx context.Context, query string, args ...any) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Conn.Exec(ctx, query, args...)
}
func (c *agentConnection) Ping(ctx context.Context) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Conn.Ping(ctx)
}
func (c *agentConnection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Conn.Close()
}

type agentRows struct {
	driver.Rows
	once    sync.Once
	release func()
}

func (r *agentRows) Close() error { err := r.Rows.Close(); r.once.Do(r.release); return err }
func (r *agentRows) Next() bool {
	ok := r.Rows.Next()
	if !ok {
		r.once.Do(r.release)
	}
	return ok
}

type agentRow struct {
	driver.Row
	once    sync.Once
	release func()
}

func (r *agentRow) Scan(dest ...any) error { defer r.once.Do(r.release); return r.Row.Scan(dest...) }
func (r *agentRow) ScanStruct(dest any) error {
	defer r.once.Do(r.release)
	return r.Row.ScanStruct(dest)
}
func (r *agentRow) Err() error {
	err := r.Row.Err()
	if err != nil {
		r.once.Do(r.release)
	}
	return err
}

func (c *agentConnection) Select(ctx context.Context, dest any, query string, args ...any) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Conn.Select(ctx, dest, query, args...)
}
