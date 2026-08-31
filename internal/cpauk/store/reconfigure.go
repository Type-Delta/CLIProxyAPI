package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

const sqliteMaximumPageCount int64 = 4_294_967_294

func (s *SQLiteStore) ReconfigureStorageBudget(ctx context.Context, maxBytes, minFreeBytes int64) error {
	if maxBytes < 0 || minFreeBytes < 0 || maxBytes == 0 && minFreeBytes == 0 {
		return fmt.Errorf("analytics storage limits cannot both be disabled")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	var pageSize, pageCount, oldMaximum int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return fmt.Errorf("read SQLite page size: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return fmt.Errorf("read SQLite page count: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA max_page_count").Scan(&oldMaximum); err != nil {
		return fmt.Errorf("read SQLite page quota: %w", err)
	}
	maximum := sqliteMaximumPageCount
	if maxBytes > 0 {
		databaseBudget := maxBytes/10*8 + maxBytes%10*8/10
		maximum = databaseBudget / pageSize
		if maximum < pageCount || maximum < 1 {
			return ErrStorageQuota
		}
	}
	if err := setMaximumPageCount(ctx, s.db, maximum); err != nil {
		return err
	}
	oldMaxBytes, oldMinFreeBytes := s.config.MaxStorageBytes, s.config.MinFreeBytes
	s.config.MaxStorageBytes, s.config.MinFreeBytes = maxBytes, minFreeBytes
	if err := s.checkQuota(0); err != nil {
		s.config.MaxStorageBytes, s.config.MinFreeBytes = oldMaxBytes, oldMinFreeBytes
		_ = setMaximumPageCount(context.Background(), s.db, oldMaximum)
		return err
	}
	return nil
}

func setMaximumPageCount(ctx context.Context, database queryRower, count int64) error {
	var applied int64
	if err := database.QueryRowContext(ctx, "PRAGMA max_page_count = "+strconv.FormatInt(count, 10)).Scan(&applied); err != nil {
		return fmt.Errorf("set SQLite page quota: %w", err)
	}
	if applied < count {
		return fmt.Errorf("SQLite page quota was not fully applied")
	}
	return nil
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
