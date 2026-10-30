package chstore

import (
	"context"
	"fmt"
)

// analyticsTables lists every analytics table that maintenance (backup,
// restore, epoch rotation, integrity checks) must cover. A test fails when a
// migration adds an analytics_ table that is missing here.
var analyticsTables = []string{"analytics_events", "analytics_outcomes", "analytics_metadata", "analytics_control", "analytics_control_heads", "analytics_rollups", "analytics_request_rollups", "analytics_retention_heads", "analytics_quota_history"}

func (s *Store) IntegrityCheck(ctx context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return ErrClosed
	}
	for _, table := range analyticsTables {
		rows, err := s.db.Conn().Query(ctx, "CHECK TABLE "+s.table(table))
		if err != nil {
			return fmt.Errorf("check analytics table %s: %w", table, err)
		}
		for rows.Next() {
			var valid uint8
			var part, message string
			if err := rows.Scan(&part, &valid, &message); err != nil {
				_ = rows.Close()
				return fmt.Errorf("read analytics check %s: %w", table, err)
			}
			if valid != 1 {
				_ = rows.Close()
				return fmt.Errorf("%w: table %s part %s: %s", ErrCorruptDatabase, table, part, message)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("check analytics table %s: %w", table, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close analytics check %s: %w", table, err)
		}
	}
	return nil
}

// ClickHouse acknowledges inserts after writing parts; there is no SQLite WAL to checkpoint.
func (s *Store) Checkpoint(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	if err := s.db.Conn().Ping(ctx); err != nil {
		return fmt.Errorf("check analytics connection: %w", err)
	}
	return nil
}

func (s *Store) Reindex(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	// Request ordinary background-sized merges. FINAL would force all parts into
	// one, including the active event partition, without improving correctness.
	for _, table := range analyticsTables {
		if err := s.db.Conn().Exec(ctx, "OPTIMIZE TABLE "+s.table(table)); err != nil {
			return fmt.Errorf("optimize analytics table %s: %w", table, err)
		}
	}
	return nil
}

func (s *Store) PreviewPurgeByKeyID(ctx context.Context, keyID string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return 0, ErrClosed
	}
	return s.countRemoval(ctx, "key_id", keyID)
}

func (s *Store) PurgeByKeyID(ctx context.Context, keyID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return 0, ErrClosed
	}
	if err := s.requireLegacyImportDone(ctx); err != nil {
		return 0, err
	}
	return s.removeHistory(ctx, "key_id", keyID)
}

func (s *Store) RollbackImport(ctx context.Context, batchID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return 0, ErrClosed
	}
	if err := s.requireLegacyImportDone(ctx); err != nil {
		return 0, err
	}
	if batchID == "" {
		return 0, fmt.Errorf("import batch ID is required")
	}
	checkpoints, err := s.loadControl(ctx, "import_checkpoints")
	if err != nil {
		return 0, err
	}
	delete(checkpoints, batchID)
	// Invalidate completion before removing facts. A failed cleanup must never
	// advertise a completed import whose event sources have already gone.
	if err = s.replaceControlMode(ctx, "import_checkpoints", checkpoints, false); err != nil {
		return 0, fmt.Errorf("remove import checkpoint: %w", err)
	}
	return s.removeHistory(ctx, "import_batch_id", batchID)
}

func (s *Store) countRemoval(ctx context.Context, column, value string) (int64, error) {
	var total uint64
	for _, table := range []string{"analytics_events", "analytics_rollups", "analytics_request_rollups"} {
		var count uint64
		if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table(table)+" FINAL WHERE is_deleted=0 AND "+column+"=?"+s.c4bTablePublicationFilter(table), value).Scan(&count); err != nil {
			return 0, fmt.Errorf("count %s: %w", table, err)
		}
		total += count
	}
	return int64(total), nil
}

// removeHistory removes response references before allowing an attempt ID to be inserted again.
// Mutations are synchronous, and the caller holds the writer lock throughout.
func (s *Store) removeHistory(ctx context.Context, column, value string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	count, err := s.countRemoval(ctx, column, value)
	if err != nil {
		return 0, err
	}
	rows, err := s.db.Conn().Query(ctx, "SELECT attempt_id FROM "+s.table("analytics_events")+" FINAL WHERE is_deleted=0 AND "+column+"=?", value)
	if err != nil {
		return 0, fmt.Errorf("read removal attempts: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	// Retention may already have removed an attempt while leaving its patch.
	// Such patches cannot belong to any currently visible event and must not
	// attach to a future re-import of the same attempt ID.
	orphans, err := s.db.Conn().Query(ctx, "SELECT DISTINCT id FROM "+s.table("analytics_outcomes")+" ARRAY JOIN attempt_ids AS id WHERE id NOT IN (SELECT attempt_id FROM "+s.table("analytics_events")+" FINAL WHERE is_deleted=0)")
	if err != nil {
		return 0, fmt.Errorf("read orphaned outcome references: %w", err)
	}
	for orphans.Next() {
		var id string
		if err := orphans.Scan(&id); err != nil {
			_ = orphans.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := orphans.Err(); err != nil {
		_ = orphans.Close()
		return 0, err
	}
	if err := orphans.Close(); err != nil {
		return 0, err
	}
	// A submitted ClickHouse mutation continues on the server after cancellation.
	// Keep the lock until all submitted mutations have completed.
	mutationCtx := context.WithoutCancel(ctx)
	for start := 0; start < len(ids); start += 1000 {
		batch := ids[start:min(start+1000, len(ids))]
		query := "ALTER TABLE " + s.table("analytics_outcomes") + " UPDATE attempt_ids=arrayFilter(x -> NOT has(?, x), attempt_ids) WHERE hasAny(attempt_ids, ?) SETTINGS mutations_sync=2"
		if err := s.db.Conn().Exec(mutationCtx, query, batch, batch); err != nil {
			return 0, fmt.Errorf("remove outcome references: %w", err)
		}
	}
	for _, table := range []string{"analytics_rollups", "analytics_request_rollups", "analytics_events"} {
		query := "ALTER TABLE " + s.table(table) + " DELETE WHERE " + column + "=? SETTINGS mutations_sync=2"
		if err := s.db.Conn().Exec(mutationCtx, query, value); err != nil {
			return 0, fmt.Errorf("remove history from %s: %w", table, err)
		}
	}
	return count, nil
}
