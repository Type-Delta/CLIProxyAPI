package chstore

import (
	"context"
	"fmt"
	"slices"
)

// Reconfigure applies live storage settings. Paths and retained bucket zones
// remain fixed for the lifetime of the store; pricing has its own update API.
func (s *Store) Reconfigure(ctx context.Context, cfg Config) error {
	if err := normalizeConfig(&cfg); err != nil {
		return err
	}
	s.mu.RLock()
	current, closed := s.config, s.db == nil
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if cfg.Path != current.Path || cfg.IdentityKeyPath != current.IdentityKeyPath || cfg.RetentionTimeZone != current.RetentionTimeZone {
		return fmt.Errorf("analytics storage paths and time zone require restart")
	}
	if cfg.MaxStorageBytes != current.MaxStorageBytes || cfg.MinFreeBytes != current.MinFreeBytes {
		if err := s.ReconfigureStorageBudget(ctx, cfg.MaxStorageBytes, cfg.MinFreeBytes); err != nil {
			return err
		}
	}
	if !slices.Equal(cfg.CatalogBindings, current.CatalogBindings) {
		s.SetCatalogBindings(cfg.CatalogBindings)
	}
	return ctx.Err()
}
