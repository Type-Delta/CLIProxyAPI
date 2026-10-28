package chstore

import (
	"context"
	"path/filepath"
	"testing"
)

func TestI2ReconfigureUsesNeutralStorageConfig(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	cfg := s.config
	cfg.MaxStorageBytes = 128 << 20
	cfg.CatalogBindings = []CatalogBinding{{Provider: "openai", Catalog: "openai"}}
	if err := s.Reconfigure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	current := s.config
	s.mu.RUnlock()
	if current.MaxStorageBytes != cfg.MaxStorageBytes || len(current.CatalogBindings) != 1 {
		t.Fatalf("configuration %+v", current)
	}
	cfg.Path = filepath.Join(t.TempDir(), "other.db")
	if err := s.Reconfigure(context.Background(), cfg); err == nil {
		t.Fatal("path change accepted without restart")
	}
}
