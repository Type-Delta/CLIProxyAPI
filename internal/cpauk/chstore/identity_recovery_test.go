package chstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

func TestUnreadableLegacySourceAllowsAbandonRecovery(t *testing.T) {
	t.Parallel()
	for _, existingKey := range []bool{false, true} {
		name := "without-key"
		if existingKey {
			name = "with-key"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			source, err := os.ReadFile("testdata/whole-corrupt-source.db")
			if err != nil {
				t.Fatal(err)
			}
			cfg := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20}
			if err = os.WriteFile(cfg.Path, source, 0600); err != nil {
				t.Fatal(err)
			}
			keyPath := filepath.Join(filepath.Dir(cfg.Path), "identity.key")
			originalKey := bytes.Repeat([]byte{0x42}, 32)
			if existingKey {
				if err = os.WriteFile(keyPath, originalKey, 0600); err != nil {
					t.Fatal(err)
				}
			}
			handle := chtest.Handle(t)
			s, err := Open(ctx, handle, cfg)
			if err != nil {
				t.Fatalf("unreadable legacy source blocked store startup: %v", err)
			}
			t.Cleanup(func() { _ = s.Close(ctx) })
			key, epoch := s.IdentityKey(), s.IdentityEpoch()
			if len(key) != 32 || !model.IsCorrelationID(epoch) {
				t.Fatal("fresh identity was not initialized")
			}
			if existingKey && !bytes.Equal(key, originalKey) {
				t.Fatal("existing local identity changed")
			}
			persistedKey, err := os.ReadFile(keyPath)
			if err != nil || !bytes.Equal(key, persistedKey) {
				t.Fatalf("identity key was not persisted: %v", err)
			}
			job, found, err := s.db.Job(ctx, c4bLegacyJob)
			if err != nil || !found || job.State != "pending" {
				t.Fatalf("startup import job=%+v found=%t err=%v", job, found, err)
			}
			// Match the runtime worker: startup succeeds, then the background import fails.
			imported := make(chan error, 1)
			go func() { imported <- RunLegacyImport(ctx, s, cfg.Path) }()
			err = <-imported
			if err == nil || !strings.Contains(err.Error(), "legacy analytics database is unreadable:") {
				t.Fatalf("import error=%v", err)
			}
			job, found, err = s.db.Job(ctx, c4bLegacyJob)
			if err != nil || !found || job.State != "failed" || !strings.Contains(job.Detail, "legacy analytics database is unreadable:") {
				t.Fatalf("failed import job=%+v found=%t err=%v", job, found, err)
			}
			if err = s.AbandonLegacyImport(ctx, "accept unreadable source loss"); err != nil {
				t.Fatal(err)
			}
			job, _, err = s.db.Job(ctx, c4bLegacyJob)
			if err != nil || job.State != "abandoned" {
				t.Fatalf("abandon job=%+v err=%v", job, err)
			}
			if _, err = s.ApplyRetention(ctx, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 1000); err != nil {
				t.Fatalf("retention after abandonment: %v", err)
			}
			actual, err := os.ReadFile(cfg.Path)
			if err != nil || !bytes.Equal(actual, source) {
				t.Fatal("corrupt source was modified")
			}
			if err = s.Close(ctx); err != nil {
				t.Fatal(err)
			}
			s, err = Open(ctx, handle, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(s.IdentityKey(), key) || s.IdentityEpoch() != epoch {
				t.Fatal("local identity changed on reopen")
			}
		})
	}
}

func TestReadableLegacyIdentityAdoptionAndMismatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20}
	legacy, err := store.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	key, epoch := legacy.IdentityKey(), legacy.IdentityEpoch()
	if err = legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(filepath.Dir(cfg.Path), "identity.key")
	if err = os.WriteFile(keyPath, bytes.Repeat([]byte{0x66}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	handle := chtest.Handle(t)
	if _, err = Open(ctx, handle, cfg); !errors.Is(err, ErrIdentityKeyMismatch) {
		t.Fatalf("readable fingerprint mismatch was not refused: %v", err)
	}
	if err = os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(ctx, handle, cfg); !errors.Is(err, ErrIdentityKeyMissing) {
		t.Fatalf("readable source without its key was not refused: %v", err)
	}
	if err = os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, handle, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(ctx) }()
	if !bytes.Equal(s.IdentityKey(), key) || s.IdentityEpoch() != epoch {
		t.Fatal("readable legacy identity was not adopted")
	}
	if err = RunLegacyImport(ctx, s, cfg.Path); err != nil {
		t.Fatal(err)
	}
	job, found, err := s.db.Job(ctx, c4bLegacyJob)
	if err != nil || !found || job.State != "done" {
		t.Fatalf("readable import job=%+v found=%t err=%v", job, found, err)
	}
	actual, err := os.ReadFile(cfg.Path)
	if err != nil || !bytes.Equal(actual, source) {
		t.Fatal("readable source was modified")
	}
}
