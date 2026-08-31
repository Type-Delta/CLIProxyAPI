package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStartNewIdentityEpochArchivesWithoutRewriting(t *testing.T) {
	directory := t.TempDir()
	config := Config{Path: filepath.Join(directory, "analytics.db"), MaxStorageBytes: 64 << 20}
	database, err := Open(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	oldEpoch := database.IdentityEpoch()
	if err := database.WriteBatch(context.Background(), loadFixtureEvents(t)[:1]); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := StartNewIdentityEpoch(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = result.Store.Close(context.Background()) }()
	if result.IdentityEpoch == oldEpoch || result.ArchivedDB == "" {
		t.Fatalf("epoch did not rotate: old=%s new=%s", oldEpoch, result.IdentityEpoch)
	}
	if _, err := os.Stat(result.ArchivedDB); err != nil {
		t.Fatalf("archived database: %v", err)
	}
	var count int64
	if err := result.Store.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&count); err != nil || count != 0 {
		t.Fatalf("new epoch count=%d err=%v", count, err)
	}
}
