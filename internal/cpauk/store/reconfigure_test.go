package store

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
)

func TestReconfigureStorageBudgetAppliesAndRollsBack(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	if err := database.ReconfigureStorageBudget(ctx, 128<<20, 0); err != nil {
		t.Fatal(err)
	}
	if database.config.MaxStorageBytes != 128<<20 || database.config.MinFreeBytes != 0 {
		t.Fatalf("budget=%d/%d", database.config.MaxStorageBytes, database.config.MinFreeBytes)
	}
	if err := database.ReconfigureStorageBudget(ctx, 1, 0); !errors.Is(err, ErrStorageQuota) {
		t.Fatalf("small budget error=%v", err)
	}
	if database.config.MaxStorageBytes != 128<<20 {
		t.Fatal("failed quota update changed active budget")
	}
	if err := database.ReconfigureStorageBudget(ctx, 128<<20, math.MaxInt64); !errors.Is(err, ErrInsufficientFreeSpace) {
		t.Fatalf("free-space update error=%v", err)
	}
	if database.config.MinFreeBytes != 0 {
		t.Fatal("failed free-space update changed active budget")
	}
}
