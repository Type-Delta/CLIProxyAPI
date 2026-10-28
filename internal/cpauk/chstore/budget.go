package chstore

import (
	"context"
	"fmt"
	"math"
	"time"
)

const budgetCacheDuration = time.Second

// checkQuota checks server storage. Callers hold the store lock.
func (s *Store) checkQuota(ctx context.Context, additional int64) error {
	s.budgetMu.Lock()
	defer s.budgetMu.Unlock()
	if s.budgetAt.IsZero() || time.Since(s.budgetAt) >= budgetCacheDuration {
		var used uint64
		if err := s.db.Conn().QueryRow(ctx, "SELECT sum(bytes_on_disk) FROM system.parts WHERE database=? AND startsWith(table, 'analytics_') AND active", s.db.Database()).Scan(&used); err != nil {
			return fmt.Errorf("inspect analytics ClickHouse parts: %w", err)
		}
		var free uint64
		if err := s.db.Conn().QueryRow(ctx, "SELECT min(free_space) FROM system.disks WHERE is_read_only=0").Scan(&free); err != nil {
			return fmt.Errorf("inspect analytics ClickHouse disks: %w", err)
		}
		s.budgetUsed = int64(min(used, uint64(math.MaxInt64)))
		s.budgetFree = free
		s.budgetReserved = 0
		s.budgetAt = time.Now()
	}
	extra := max(additional, 0)
	if s.budgetUsed > math.MaxInt64-s.budgetReserved || s.budgetUsed+s.budgetReserved > math.MaxInt64-extra {
		return ErrStorageQuota
	}
	used := s.budgetUsed + s.budgetReserved + extra
	if s.config.MaxStorageBytes > 0 && used >= s.config.MaxStorageBytes {
		return ErrStorageQuota
	}
	reserve := uint64(s.config.MinFreeBytes) + uint64(s.budgetReserved) + uint64(extra)
	if s.budgetFree <= reserve {
		return ErrInsufficientFreeSpace
	}
	return nil
}
func (s *Store) reserveBudget(additional int64) {
	s.budgetMu.Lock()
	defer s.budgetMu.Unlock()
	s.budgetReserved += max(additional, 0)
}
func (s *Store) ReconfigureStorageBudget(ctx context.Context, maxBytes, minFreeBytes int64) error {
	if maxBytes < 0 || minFreeBytes < 0 || maxBytes == 0 && minFreeBytes == 0 {
		return fmt.Errorf("analytics storage limits cannot both be disabled")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	oldMax, oldMin := s.config.MaxStorageBytes, s.config.MinFreeBytes
	s.config.MaxStorageBytes, s.config.MinFreeBytes = maxBytes, minFreeBytes
	s.budgetMu.Lock()
	s.budgetAt = time.Time{}
	s.budgetMu.Unlock()
	if err := s.checkQuota(ctx, 0); err != nil {
		s.config.MaxStorageBytes, s.config.MinFreeBytes = oldMax, oldMin
		return err
	}
	return nil
}
