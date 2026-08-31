package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func (s *SQLiteStore) LoadImportCheckpoint(ctx context.Context, batchID string) ([]byte, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, false, ErrClosed
	}
	var digest string
	err := s.db.QueryRowContext(ctx, "SELECT digest FROM import_checkpoints WHERE batch_id = ?", batchID).Scan(&digest)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load import checkpoint: %w", err)
	}
	return []byte(digest), true, nil
}

func (s *SQLiteStore) SaveImportCheckpoint(ctx context.Context, batchID, sourceKind, fingerprint string, offset int64, chunk int, checkpoint []byte, completed bool, counters [5]int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO import_checkpoints
(batch_id,source_kind,source_fingerprint,source_offset,chunk,digest,completed,rows_read,transformed,inserted,skipped,rejected,updated_at_ns)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(batch_id) DO UPDATE SET source_offset=excluded.source_offset,chunk=excluded.chunk,
digest=excluded.digest,completed=excluded.completed,rows_read=excluded.rows_read,
transformed=excluded.transformed,inserted=excluded.inserted,skipped=excluded.skipped,
rejected=excluded.rejected,updated_at_ns=excluded.updated_at_ns`, batchID, sourceKind, fingerprint,
		offset, chunk, string(checkpoint), completed, counters[0], counters[1], counters[2], counters[3], counters[4], time.Now().UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("save import checkpoint: %w", err)
	}
	return nil
}
