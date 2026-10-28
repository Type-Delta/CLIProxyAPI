package chstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestBusyLegacyIdentityNeverCreatesReplacement(t *testing.T) {
	t.Parallel()
	for _, haveKey := range []bool{false, true} {
		name := "missing-key"
		if haveKey {
			name = "existing-key"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cfg := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20}
			legacy, err := store.Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			originalKey, originalEpoch := legacy.IdentityKey(), legacy.IdentityEpoch()
			if err = legacy.Close(ctx); err != nil {
				t.Fatal(err)
			}
			keyPath := filepath.Join(filepath.Dir(cfg.Path), "identity.key")
			if !haveKey {
				if err = os.Remove(keyPath); err != nil {
					t.Fatal(err)
				}
			}
			blocker, err := sql.Open("sqlite", cfg.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = blocker.Close() }()
			conn, err := blocker.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if _, err = conn.ExecContext(ctx, "PRAGMA journal_mode=DELETE"); err != nil {
				t.Fatal(err)
			}
			if _, err = conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = conn.ExecContext(ctx, "ROLLBACK") }()
			handle := chtest.Handle(t)
			s, err := Open(ctx, handle, cfg)
			if err == nil {
				defer func() { _ = s.Close(ctx) }()
				_, statErr := os.Stat(keyPath)
				t.Fatalf("Open accepted SQLITE_BUSY and persisted a new epoch; key exists=%t", statErr == nil)
			}
			var sqliteErr *sqlite.Error
			if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY {
				t.Fatalf("original SQLite busy reason lost: %v", err)
			}
			var classified interface {
				Category() string
				Permanent() bool
			}
			if !errors.As(err, &classified) || classified.Category() != "legacy_identity_busy" || classified.Permanent() {
				t.Fatalf("busy startup reason unavailable to health/retry: %v", err)
			}
			key, readErr := os.ReadFile(keyPath)
			if haveKey {
				if readErr != nil || !bytes.Equal(key, originalKey) {
					t.Fatalf("existing key changed: %v", readErr)
				}
			} else if !errors.Is(readErr, os.ErrNotExist) {
				t.Fatalf("replacement key created while source was locked: %v", readErr)
			}
			db, err := handle.Get(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var identities uint64
			if err = db.Conn().QueryRow(ctx, "SELECT count() FROM `"+db.Database()+"`.analytics_metadata FINAL WHERE key IN ('identity_fingerprint','identity_epoch') AND is_deleted=0").Scan(&identities); err != nil {
				t.Fatal(err)
			}
			if identities != 0 {
				t.Fatalf("failed startup persisted %d identity entries", identities)
			}
			if _, err = conn.ExecContext(ctx, "ROLLBACK"); err != nil {
				t.Fatal(err)
			}
			if !haveKey {
				if _, err = Open(ctx, handle, cfg); !errors.Is(err, ErrIdentityKeyMissing) {
					t.Fatalf("readable source did not require original key: %v", err)
				}
				if err = os.WriteFile(keyPath, originalKey, 0600); err != nil {
					t.Fatal(err)
				}
			}
			s, err = Open(ctx, handle, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close(ctx) }()
			if !bytes.Equal(s.IdentityKey(), originalKey) || s.IdentityEpoch() != originalEpoch {
				t.Fatal("retry did not adopt original identity")
			}
		})
	}
}

func TestNonCorruptionLegacyReadErrorDoesNotCreateIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20}
	db, err := sql.Open("sqlite", cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "CREATE TABLE unrelated(value TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, chtest.Handle(t), cfg)
	if err == nil {
		defer func() { _ = s.Close(ctx) }()
		t.Fatal("generic SQLite error authorized a new identity")
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != sqlite3.SQLITE_ERROR {
		t.Fatalf("source read error lost: %v", err)
	}
	if _, err = os.Stat(filepath.Join(filepath.Dir(cfg.Path), "identity.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source read failure created a key: %v", err)
	}
}

func TestTruncatedLegacySourceAllowsAbandonRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20}
	legacy, err := store.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(original) <= 100 {
		t.Fatal("fixture database lacks pages")
	}
	// Keep the complete SQLite header but remove every database page body.
	if err = os.WriteFile(cfg.Path, original[:100], 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(filepath.Dir(cfg.Path), "identity.key")); err != nil {
		t.Fatal(err)
	}
	_, _, err = legacyIdentity(ctx, cfg.Path)
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != sqlite3.SQLITE_CORRUPT {
		t.Fatalf("truncation fixture did not report SQLite corruption: %v", err)
	}
	s, err := Open(ctx, chtest.Handle(t), cfg)
	if err != nil {
		t.Fatalf("confirmed corruption blocked recovery: %v", err)
	}
	defer func() { _ = s.Close(ctx) }()
	if err = RunLegacyImport(ctx, s, cfg.Path); err == nil {
		t.Fatal("truncated source import succeeded")
	}
	job, found, err := s.db.Job(ctx, c4bLegacyJob)
	if err != nil || !found || job.State != "failed" {
		t.Fatalf("failed job=%+v found=%t err=%v", job, found, err)
	}
	if err = s.AbandonLegacyImport(ctx, "accept truncated source loss"); err != nil {
		t.Fatal(err)
	}
}
