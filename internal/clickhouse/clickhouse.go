package clickhouse

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	log "github.com/sirupsen/logrus"
)

var ErrNotConfigured = errors.New("clickhouse: not configured")
var databasePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

//go:embed migrations/*.sql
var coreFiles embed.FS

type Config struct{ DSN, Database string }
type Handle struct {
	cfg             Config
	mu              sync.Mutex
	db              *DB
	closed          bool
	agent           agentState
	healthMu        sync.Mutex
	healthCheckedAt time.Time
	healthErr       error
	healthChecking  bool
}
type DB struct {
	conn       driver.Conn
	database   string
	mu         sync.Mutex
	jobsMu     sync.Mutex
	jobVersion uint64
}

func NewHandle(cfg Config) *Handle {
	if cfg.Database == "" {
		cfg.Database = "cpa"
	}
	return &Handle{cfg: cfg}
}
func (h *Handle) Configured() bool { return strings.TrimSpace(h.cfg.DSN) != "" }
func (h *Handle) Get(ctx context.Context) (*DB, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.Configured() {
		return nil, ErrNotConfigured
	}
	if h.closed {
		return nil, errors.New("clickhouse: handle closed")
	}
	if h.db != nil {
		return h.db, nil
	}
	if !databasePattern.MatchString(h.cfg.Database) {
		return nil, errors.New("clickhouse: invalid database identifier")
	}
	opts, err := ch.ParseDSN(h.cfg.DSN)
	if err != nil {
		return nil, errors.New("clickhouse: invalid DSN")
	}
	opts.Auth.Database = "default"
	conn, err := ch.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: open connection: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = conn.Close()
		}
	}()
	if err = conn.Ping(ctx); err != nil {
		return nil, fmt.Errorf("clickhouse: connect: %w", err)
	}
	if err = conn.Exec(ctx, "CREATE DATABASE IF NOT EXISTS `"+h.cfg.Database+"`"); err != nil {
		return nil, fmt.Errorf("clickhouse: create database: %w", err)
	}
	db := &DB{conn: conn, database: h.cfg.Database}
	if err = db.bootstrap(ctx); err != nil {
		return nil, err
	}
	h.db = db
	h.healthMu.Lock()
	h.healthCheckedAt = time.Now()
	h.healthErr = nil
	h.healthMu.Unlock()
	success = true
	log.WithField("database", h.cfg.Database).Info("clickhouse connected")
	return db, nil
}
func (h *Handle) Close() (err error) {
	defer func() { err = errors.Join(err, h.CloseAgent()) }()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	if h.db != nil {
		err := h.db.conn.Close()
		h.db = nil
		return err
	}
	return nil
}
func (db *DB) Conn() driver.Conn              { return db.conn }
func (db *DB) Database() string               { return db.database }
func (db *DB) Ping(ctx context.Context) error { return db.conn.Ping(ctx) }
func (db *DB) bootstrap(ctx context.Context) error {
	sql := `CREATE TABLE IF NOT EXISTS ` + db.ident() + `.schema_migrations (component String COMMENT 'Component owning the migration', version UInt32 COMMENT 'Migration version within the component', name String COMMENT 'Migration file name', checksum String COMMENT 'SHA-256 checksum of the migration file', applied_at DateTime64(3, 'UTC') COMMENT 'Time the migration was applied') ENGINE = MergeTree ORDER BY (component, version) COMMENT 'Applied schema migrations and their checksums'`
	if err := db.conn.Exec(ctx, sql); err != nil {
		return fmt.Errorf("clickhouse: bootstrap migrations: %w", err)
	}
	_, err := db.Migrate(ctx, "core", coreFiles)
	return err
}
func (db *DB) ident() string { return "`" + db.database + "`" }

// CheckHealth probes at most once every five seconds without holding the pool
// ownership mutex. Concurrent readers use the last completed health result.
func (h *Handle) CheckHealth(ctx context.Context) error {
	db, err := h.Get(ctx)
	if err != nil {
		return err
	}
	h.healthMu.Lock()
	if h.healthChecking || time.Since(h.healthCheckedAt) < 5*time.Second {
		err = h.healthErr
		h.healthMu.Unlock()
		return err
	}
	h.healthChecking = true
	h.healthMu.Unlock()
	err = db.Ping(ctx)
	h.healthMu.Lock()
	if ctx.Err() == nil {
		h.healthErr = err
		h.healthCheckedAt = time.Now()
	}
	h.healthChecking = false
	h.healthMu.Unlock()
	return err
}
