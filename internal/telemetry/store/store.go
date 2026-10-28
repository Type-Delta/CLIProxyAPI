// Package store manages the ClickHouse telemetry schema and retention.
package store

import (
	"context"
	"embed"
	"fmt"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
)

//go:embed migrations/*.sql
var migrations embed.FS

const LegacyJob = "legacy-import:request-logs"

type Store struct {
	db *clickhouse.DB
	mu sync.Mutex
}

func (s *Store) Lock()   { s.mu.Lock() }
func (s *Store) Unlock() { s.mu.Unlock() }

// Open applies the telemetry schema and schedules legacy import on first creation.
func Open(ctx context.Context, db *clickhouse.DB, retentionDays int) (*Store, error) {
	if _, err := db.MigrateWithJob(ctx, "telemetry", migrations, LegacyJob); err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.SetRetention(ctx, retentionDays); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) DB() *clickhouse.DB { return s.db }

// SetRetention updates TTLs for request-owned rows. Content is removed by GC.
func (s *Store) SetRetention(ctx context.Context, days int) error {
	if days <= 0 {
		return fmt.Errorf("telemetry retention must be positive")
	}
	for _, table := range []struct{ name, column string }{{"requests", "started_at"}, {"attempts", "request_started_at"}, {"request_contexts", "started_at"}} {
		var engine string
		if err := s.db.Conn().QueryRow(ctx, "SELECT engine_full FROM system.tables WHERE database=? AND name=?", s.db.Database(), table.name).Scan(&engine); err != nil {
			return fmt.Errorf("read telemetry retention for %s: %w", table.name, err)
		}
		expected := fmt.Sprintf("TTL %s + toIntervalDay(%d)", table.column, days)
		if strings.Contains(engine, expected) {
			continue
		}
		query := fmt.Sprintf("ALTER TABLE `%s`.%s MODIFY TTL %s + INTERVAL %d DAY", s.db.Database(), table.name, table.column, days)
		if err := s.db.Conn().Exec(ctx, query); err != nil {
			return fmt.Errorf("set telemetry retention for %s: %w", table.name, err)
		}
	}
	return nil
}

// GC removes old unreferenced entries and then unreferenced content. Mutations
// are synchronous so content is never removed while a surviving entry needs it.
func (s *Store) GC(ctx context.Context, days int) error {
	if days <= 0 {
		return fmt.Errorf("telemetry retention must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	db := "`" + s.db.Database() + "`."
	entries := fmt.Sprintf("ALTER TABLE %stranscript_entries DELETE WHERE first_observed_at < now() - INTERVAL %d DAY AND entry_id NOT IN (SELECT arrayJoin(entry_ids) FROM %srequest_contexts) AND first_request_id NOT IN (SELECT request_id FROM %srequests) SETTINGS mutations_sync=2", db, days, db, db)
	if err := s.db.Conn().Exec(ctx, entries); err != nil {
		return fmt.Errorf("telemetry entry GC: %w", err)
	}
	content := fmt.Sprintf("ALTER TABLE %stranscript_content DELETE WHERE first_observed_at < now() - INTERVAL %d DAY AND content_id NOT IN (SELECT content_id FROM %stranscript_entries) AND content_id NOT IN (SELECT arrayJoin(content_ids) FROM %srequest_contexts) AND content_id NOT IN (SELECT envelope_content_id FROM %srequests) AND content_id NOT IN (SELECT envelope_content_id FROM %sattempts) SETTINGS mutations_sync=2", db, days, db, db, db, db)
	if err := s.db.Conn().Exec(ctx, content); err != nil {
		return fmt.Errorf("telemetry content GC: %w", err)
	}
	return nil
}
