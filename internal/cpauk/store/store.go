package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version  int
	name     string
	sql      string
	checksum string
}

type SQLiteStore struct {
	mu              sync.RWMutex
	db              *sql.DB
	config          Config
	identityKey     []byte
	identityEpoch   string
	currentSchema   int
	retentionCutoff time.Time
}

func Open(ctx context.Context, config Config) (*SQLiteStore, error) {
	if err := config.normalize(); err != nil {
		return nil, err
	}
	if config.CursorCodec == nil {
		cursorKey := make([]byte, 32)
		if _, err := rand.Read(cursorKey); err != nil {
			return nil, fmt.Errorf("generate analytics cursor key: %w", err)
		}
		codec, err := model.NewCursorCodec(cursorKey)
		if err != nil {
			return nil, err
		}
		config.CursorCodec = codec
	}
	if err := os.MkdirAll(filepath.Dir(config.Path), 0o700); err != nil {
		return nil, fmt.Errorf("create analytics directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(config.Path), 0o700); err != nil {
		return nil, fmt.Errorf("set analytics directory permissions: %w", err)
	}
	_, statErr := os.Stat(config.Path)
	newDatabase := errors.Is(statErr, fs.ErrNotExist)
	if statErr != nil && !newDatabase {
		return nil, fmt.Errorf("inspect analytics database: %w", statErr)
	}
	identityKey, err := loadIdentityKey(config.IdentityKeyPath, newDatabase)
	if err != nil {
		return nil, err
	}
	db, err := openDatabase(ctx, config)
	if err != nil {
		return nil, err
	}
	store := &SQLiteStore{db: db, config: config, identityKey: identityKey}
	if err := store.initialize(ctx, newDatabase); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func openDatabase(ctx context.Context, config Config) (*sql.DB, error) {
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(config.Path)}).String()
	uri += "?_pragma=busy_timeout(2000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, fmt.Errorf("open analytics SQLite database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect analytics SQLite database: %w", err)
	}
	if err := os.Chmod(config.Path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set analytics database permissions: %w", err)
	}
	var pageSize int64
	if err := db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read SQLite page size: %w", err)
	}
	if config.MaxStorageBytes > 0 {
		databaseBudget := config.MaxStorageBytes * 8 / 10
		maxPages := databaseBudget / pageSize
		if maxPages < 1 {
			_ = db.Close()
			return nil, fmt.Errorf("analytics storage maximum is smaller than one SQLite page")
		}
		if _, err := db.ExecContext(ctx, "PRAGMA max_page_count = "+strconv.FormatInt(maxPages, 10)); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("set SQLite page quota: %w", err)
		}
	}
	return db, nil
}

func (s *SQLiteStore) initialize(ctx context.Context, newDatabase bool) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at_ns INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	var current int
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&current); err != nil {
		return fmt.Errorf("read analytics schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("%w: database version %d, supported %d", ErrUnsupportedSchema, current, len(migrations))
	}
	for _, item := range migrations {
		if item.version <= current {
			var checksum string
			if err := s.db.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE version = ?", item.version).Scan(&checksum); err != nil {
				return fmt.Errorf("read migration %d checksum: %w", item.version, err)
			}
			if checksum != item.checksum {
				return fmt.Errorf("%w at version %d", ErrMigrationChecksum, item.version)
			}
			continue
		}
		if !newDatabase {
			backupPath := fmt.Sprintf("%s.pre-migration-v%d", s.config.Path, item.version)
			if err := s.backupDatabase(ctx, backupPath); err != nil {
				return fmt.Errorf("backup before migration %d: %w", item.version, err)
			}
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", item.version, err)
		}
		if _, err := tx.ExecContext(ctx, item.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", item.version, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, name, checksum, applied_at_ns) VALUES (?, ?, ?, ?)", item.version, item.name, item.checksum, time.Now().UTC().UnixNano()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", item.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", item.version, err)
		}
		current = item.version
	}
	s.currentSchema = current
	fingerprint, err := model.IdentityKeyFingerprint(s.identityKey)
	if err != nil {
		return err
	}
	var identityMetadata int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM analytics_metadata
WHERE key IN ('identity_fingerprint','identity_epoch')`).Scan(&identityMetadata); err != nil {
		return fmt.Errorf("inspect analytics identity metadata: %w", err)
	}
	initializeIdentity := newDatabase
	if identityMetadata == 0 && !newDatabase {
		var sensitiveRows int64
		if err := s.db.QueryRowContext(ctx, `SELECT
(SELECT COUNT(*) FROM events)+(SELECT COUNT(*) FROM rollups)+(SELECT COUNT(*) FROM request_rollups)+
(SELECT COUNT(*) FROM import_checkpoints)`).Scan(&sensitiveRows); err != nil {
			return fmt.Errorf("inspect interrupted analytics initialization: %w", err)
		}
		if sensitiveRows != 0 {
			return fmt.Errorf("%w: identity metadata is absent from a populated database", ErrIdentityKeyMissing)
		}
		initializeIdentity = true
	}
	if identityMetadata != 0 && identityMetadata != 2 {
		return fmt.Errorf("%w: identity metadata is incomplete", ErrIdentityKeyMismatch)
	}
	if initializeIdentity {
		epoch, err := model.NewCorrelationID()
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "INSERT INTO analytics_metadata(key, value) VALUES ('identity_fingerprint', ?), ('identity_epoch', ?)", fingerprint, epoch); err != nil {
			return fmt.Errorf("record analytics identity: %w", err)
		}
		s.identityEpoch = epoch
	} else {
		var storedFingerprint string
		if err := s.db.QueryRowContext(ctx, "SELECT value FROM analytics_metadata WHERE key = 'identity_fingerprint'").Scan(&storedFingerprint); err != nil {
			return fmt.Errorf("read analytics identity fingerprint: %w", err)
		}
		if storedFingerprint != fingerprint {
			return ErrIdentityKeyMismatch
		}
		if err := s.db.QueryRowContext(ctx, "SELECT value FROM analytics_metadata WHERE key = 'identity_epoch'").Scan(&s.identityEpoch); err != nil {
			return fmt.Errorf("read analytics identity epoch: %w", err)
		}
	}
	if err := s.integrityCheck(ctx); err != nil {
		return err
	}
	book, _, err := loadPricingRules(ctx, s.db)
	if err != nil {
		return err
	}
	if len(book.Rules) != 0 {
		s.config.PriceBook = book
	}
	rows, err := s.db.QueryContext(ctx, "SELECT grain, completed_cutoff_ns FROM retention_state WHERE grain IN ('raw', 'raw_checkpoint')")
	if err != nil {
		return fmt.Errorf("read analytics retention state: %w", err)
	}
	for rows.Next() {
		var grain string
		var cutoffNS int64
		if err = rows.Scan(&grain, &cutoffNS); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan analytics retention state: %w", err)
		}
		cutoff := time.Unix(0, cutoffNS).UTC()
		if grain == "raw_checkpoint" {
			cutoff = cutoff.Add(time.Nanosecond)
		}
		s.advanceEventRetentionFloor(cutoff)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read analytics retention state: %w", err)
	}
	if err = rows.Close(); err != nil {
		return fmt.Errorf("close analytics retention state: %w", err)
	}
	return s.checkQuota(0)
}

func (s *SQLiteStore) advanceEventRetentionFloor(cutoff time.Time) {
	if cutoff.After(s.retentionCutoff) {
		s.retentionCutoff = cutoff
	}
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	result := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		var version int
		if _, err := fmt.Sscanf(entry.Name(), "%03d_", &version); err != nil || version != len(result)+1 {
			return nil, fmt.Errorf("migration %q is not monotonically numbered", entry.Name())
		}
		data, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		digest := sha256.Sum256(data)
		result = append(result, migration{version: version, name: entry.Name(), sql: string(data), checksum: hex.EncodeToString(digest[:])})
	}
	return result, nil
}

func loadIdentityKey(path string, create bool) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != 32 {
			return nil, fmt.Errorf("analytics identity key has invalid length")
		}
		return data, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read analytics identity key: %w", err)
	}
	if !create {
		return nil, ErrIdentityKeyMissing
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate analytics identity key: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create analytics identity key: %w", err)
	}
	if _, err := file.Write(key); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write analytics identity key: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("sync analytics identity key: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close analytics identity key: %w", err)
	}
	return key, nil
}

func (s *SQLiteStore) IdentityKey() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.identityKey)
}

func (s *SQLiteStore) IdentityKeyArray() [32]byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result [32]byte
	copy(result[:], s.identityKey)
	return result
}

func (s *SQLiteStore) IdentityEpoch() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.identityEpoch
}

func (s *SQLiteStore) SchemaVersion() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentSchema
}

func (s *SQLiteStore) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	done := make(chan error, 1)
	db := s.db
	s.db = nil
	go func() { done <- db.Close() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *SQLiteStore) checkQuota(additional int64) error {
	total, err := s.storageUsage()
	if err != nil {
		return err
	}
	total += additional
	if s.config.MaxStorageBytes > 0 && total >= s.config.MaxStorageBytes {
		return ErrStorageQuota
	}
	free, err := availableBytes(filepath.Dir(s.config.Path))
	if err != nil {
		return fmt.Errorf("inspect analytics free space: %w", err)
	}
	if free <= uint64(s.config.MinFreeBytes)+uint64(max64(additional, 0)) {
		return ErrInsufficientFreeSpace
	}
	return nil
}

func (s *SQLiteStore) storageUsage() (int64, error) {
	directory := filepath.Dir(s.config.Path)
	prefix := filepath.Base(s.config.Path)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0, fmt.Errorf("inspect analytics storage directory: %w", err)
	}
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return 0, fmt.Errorf("inspect analytics storage file: %w", err)
		}
		total += info.Size()
	}
	return total, nil
}

func max64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func (s *SQLiteStore) price(event model.Event) (aggregate.PriceResult, error) {
	return s.config.PriceBook.Price(event)
}
