package chstore

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Store borrows the process connection pool. mu serializes analytics mutations.
type Store struct {
	mu              sync.RWMutex
	db              *clickhouse.DB
	config          Config
	identityKey     []byte
	identityEpoch   string
	currentSchema   int
	retentionCutoff time.Time
	version         atomic.Uint64
	budgetMu        sync.Mutex
	budgetAt        time.Time
	budgetUsed      int64
	budgetFree      uint64
	budgetReserved  int64
}

func Open(ctx context.Context, handle *clickhouse.Handle, config Config) (*Store, error) {
	if err := normalizeConfig(&config); err != nil {
		return nil, err
	}
	if handle == nil {
		return nil, clickhouse.ErrNotConfigured
	}
	if config.CursorCodec == nil {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate analytics cursor key: %w", err)
		}
		codec, err := model.NewCursorCodec(key)
		if err != nil {
			return nil, err
		}
		config.CursorCodec = codec
	}
	db, err := handle.Get(ctx)
	if err != nil {
		return nil, err
	}
	files, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	result, err := db.MigrateWithJob(ctx, "analytics", files, "legacy-import:analytics")
	if err != nil {
		return nil, fmt.Errorf("migrate analytics: %w", err)
	}
	s := &Store{db: db, config: config, currentSchema: result.ToVersion}
	if err := s.recoverRestore(ctx); err != nil {
		return nil, fmt.Errorf("recover analytics restore: %w", err)
	}
	// Continue version ordering across restarts even when the wall clock moved back.
	for _, table := range []string{"analytics_events", "analytics_metadata", "analytics_control", "analytics_control_heads", "analytics_rollups", "analytics_request_rollups"} {
		var maximum uint64
		if err := db.Conn().QueryRow(ctx, "SELECT max(version) FROM "+s.table(table)).Scan(&maximum); err != nil {
			return nil, fmt.Errorf("read analytics version: %w", err)
		}
		if maximum > s.version.Load() {
			s.version.Store(maximum)
		}
	}
	var sequence uint64
	if err := db.Conn().QueryRow(ctx, "SELECT max(sequence) FROM "+s.table("analytics_outcomes")).Scan(&sequence); err != nil {
		return nil, err
	}
	if sequence > s.version.Load() {
		s.version.Store(sequence)
	}
	if err := s.initializeIdentity(ctx); err != nil {
		return nil, err
	}
	zone, found, err := s.metadata(ctx, "retention_time_zone")
	if err != nil {
		return nil, err
	}
	if found && zone != config.RetentionTimeZone {
		var count uint64
		if err := db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table("analytics_rollups")+" FINAL WHERE is_deleted=0"+s.c4bTablePublicationFilter("analytics_rollups")).Scan(&count); err != nil {
			return nil, err
		}
		if count > 0 {
			return nil, ZoneMismatchError{Stored: zone, Configured: config.RetentionTimeZone}
		}
	}
	if !found || zone != config.RetentionTimeZone {
		if err := s.setMetadata(ctx, "retention_time_zone", config.RetentionTimeZone); err != nil {
			return nil, err
		}
	}
	pricing, err := s.loadControl(ctx, "pricing")
	if err != nil {
		return nil, fmt.Errorf("read analytics pricing snapshot: %w", err)
	}
	if raw, found := pricing["snapshot"]; found {
		var snapshot PricingSnapshot
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			return nil, fmt.Errorf("decode analytics pricing snapshot: %w", err)
		}
		s.config.PriceBook = aggregate.PriceBook{Rules: snapshot.Rules, KiroUSDPerCredit: snapshot.KiroUSDPerCredit}
	}
	for _, key := range []string{"retention_raw_cutoff_ns", "retention_raw_checkpoint_ns"} {
		value, found, err := s.metadata(ctx, key)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		ns, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("decode analytics %s: %w", key, err)
		}
		cutoff := time.Unix(0, ns).UTC()
		if key == "retention_raw_checkpoint_ns" {
			cutoff = cutoff.Add(time.Nanosecond)
		}
		if cutoff.After(s.retentionCutoff) {
			s.retentionCutoff = cutoff
		}
	}
	// Opening remains possible while over budget, so maintenance can reclaim space.
	return s, nil
}

func (s *Store) table(name string) string {
	return "`" + strings.ReplaceAll(s.db.Database(), "`", "``") + "`.`" + strings.ReplaceAll(name, "`", "``") + "`"
}
func (s *Store) nextVersion() uint64 {
	for {
		old := s.version.Load()
		next := max(old+1, uint64(time.Now().UnixNano()))
		if s.version.CompareAndSwap(old, next) {
			return next
		}
	}
}
func (s *Store) IdentityKey() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.identityKey)
}
func (s *Store) IdentityKeyArray() [32]byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var key [32]byte
	copy(key[:], s.identityKey)
	return key
}
func (s *Store) IdentityEpoch() string { s.mu.RLock(); defer s.mu.RUnlock(); return s.identityEpoch }
func (s *Store) SchemaVersion() int    { s.mu.RLock(); defer s.mu.RUnlock(); return s.currentSchema }
func (s *Store) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.mu.TryLock() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for !s.mu.TryLock() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.db = nil
	return nil
}
func (s *Store) SetCatalogBindings(bindings []CatalogBinding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config.CatalogBindings = canonicalBindings(bindings)
}
