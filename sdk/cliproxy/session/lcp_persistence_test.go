package session

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
)

func TestMerklePrefixKVPersistencePreservesGenerationAndCompaction(t *testing.T) {
	t.Cleanup(func() {
		if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
			t.Error(errShutdown)
		}
	})
	if errConfigure := kvstore.ConfigureLocal(true, filepath.Join(t.TempDir(), "kv.db")); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	cfg := MerklePrefixMatcherConfig{TTL: time.Hour, Persistent: true}
	first := NewMerklePrefixMatcherWithConfig(cfg)
	first.accessCounter = 1000
	fingerprints := []string{"system", "old-user", "old-assistant", "tail-user", "tail-assistant"}
	bound := first.BindFingerprintsWithContext("caller:model", fingerprints, fingerprints[1:], "environment", 2, "account-b")
	compacted := []string{"new-system", "summary", "tail-user", "tail-assistant", "next-user"}
	compaction := first.BindFingerprintsWithContext("caller:model", compacted, compacted[1:], "environment", 2, "account-b")
	if !compaction.IsCompaction || compaction.ParentSessionID != bound.SessionID {
		t.Fatalf("fixture did not compact: %#v", compaction)
	}
	first.Stop()

	restored := NewMerklePrefixMatcherWithConfig(cfg)
	t.Cleanup(restored.Stop)
	match, ok := restored.MatchFingerprintsWithContext("caller:model", compacted, compacted[1:], "environment", 2)
	if !ok || match.AuthID != "account-b" || match.SessionID != compaction.SessionID || !match.IsCompaction || match.ParentSessionID != bound.SessionID {
		t.Fatalf("compaction continuity lost on restore: %#v, %v", match, ok)
	}
	if match.AccessNumber <= compaction.AccessNumber {
		t.Fatalf("restore reset access generation: %d <= %d", match.AccessNumber, compaction.AccessNumber)
	}
	if restored.RemoveFingerprintsBefore("caller:model", compacted, "account-b", compaction.AccessNumber) {
		t.Fatal("late failure deleted a refreshed post-reload binding")
	}
	restored.InvalidateAuth("account-b")
	afterInvalidation := NewMerklePrefixMatcherWithConfig(cfg)
	t.Cleanup(afterInvalidation.Stop)
	if got, ok := afterInvalidation.MatchFingerprintsWithContext("caller:model", compacted, compacted[1:], "environment", 2); ok {
		t.Fatalf("invalidated account resurrected after restart: %#v", got)
	}
}
