package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestVersionZeroUpgradeBacksUpAndInitializesIdentity(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	path := filepath.Join(directory, "analytics.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, "CREATE TABLE legacy_marker(value TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "identity.key"), make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(ctx, Config{Path: path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	if store.SchemaVersion() != 1 || store.IdentityEpoch() == "" {
		t.Fatalf("schema=%d epoch=%q", store.SchemaVersion(), store.IdentityEpoch())
	}
	if _, err := os.Stat(path + ".pre-migration-v1"); err != nil {
		t.Fatalf("pre-migration backup: %v", err)
	}
}

func TestInterruptedIdentityInitializationRecoversOnlyWhenEmpty(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	config := Config{Path: filepath.Join(directory, "analytics.db"), MaxStorageBytes: 64 << 20}
	database, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, "DELETE FROM analytics_metadata"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, config)
	if err != nil {
		t.Fatalf("recover empty initialization: %v", err)
	}
	if database.IdentityEpoch() == "" {
		t.Fatal("recovered identity epoch is empty")
	}
	if err := database.WriteBatch(ctx, loadFixtureEvents(t)[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, "DELETE FROM analytics_metadata"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, config); !errors.Is(err, ErrIdentityKeyMissing) {
		t.Fatalf("populated database without identity metadata error=%v", err)
	}
}

func TestFreeSpaceReserveRejectsOpen(t *testing.T) {
	_, err := Open(context.Background(), Config{Path: filepath.Join(t.TempDir(), "analytics.db"),
		MaxStorageBytes: 64 << 20, MinFreeBytes: math.MaxInt64})
	if !errors.Is(err, ErrInsufficientFreeSpace) {
		t.Fatalf("free-space error=%v", err)
	}
}
