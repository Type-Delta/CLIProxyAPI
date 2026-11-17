package auth

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
)

func TestSessionCacheKVAliasesProvenanceAndInvalidation(t *testing.T) {
	t.Cleanup(func() {
		if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
			t.Error(errShutdown)
		}
	})
	if errConfigure := kvstore.ConfigureLocal(true, filepath.Join(t.TempDir(), "kv.db")); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	first := newSessionCache(time.Hour, 8, true)
	t.Cleanup(first.Stop)
	boundAt := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	first.setAliases("account-b", sessionBinding{sessionID: "parent", provider: "codex", boundAt: boundAt, requestID: "original-request"}, "codex::parent::model-a", "codex::alias::model-a")
	first.setSessionAliases("account-c", "codex", "parent", "codex::parent::model-b")
	first.Touch("codex::parent::model-a", "account-b")

	restored := newSessionCache(time.Hour, 8, true)
	t.Cleanup(restored.Stop)
	if authID, ok := restored.Get("codex::alias::model-a"); !ok || authID != "account-b" {
		t.Fatalf("restored alias = %q, %v", authID, ok)
	}
	if authID, gotTime, requestID, ok := restored.LookupBinding("codex::parent::model-a"); !ok || authID != "account-b" || !gotTime.Equal(boundAt) || requestID != "original-request" {
		t.Fatalf("restored provenance = %q, %v, %q, %v", authID, gotTime, requestID, ok)
	}
	if got := restored.latestSessionAuth("parent", map[string]struct{}{"codex": {}}); got != "account-c" {
		t.Fatalf("TTL refresh changed selection order: %q", got)
	}
	// A late update from the replaced cache must not resurrect an old binding.
	first.Set("late-session", "stale-account")
	restored.InvalidateAuth("account-b")
	afterInvalidation := newSessionCache(time.Hour, 8, true)
	t.Cleanup(afterInvalidation.Stop)
	for _, key := range []string{"codex::parent::model-a", "codex::alias::model-a", "late-session"} {
		if got, ok := afterInvalidation.Get(key); ok {
			t.Fatalf("invalidated/stale binding %q was restored as %q", key, got)
		}
	}
}

func TestSessionCacheKVExpiryAndEviction(t *testing.T) {
	t.Cleanup(func() {
		if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
			t.Error(errShutdown)
		}
	})
	if errConfigure := kvstore.ConfigureLocal(true, filepath.Join(t.TempDir(), "kv.db")); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	first := newSessionCache(time.Hour, 2, true)
	t.Cleanup(first.Stop)
	first.Set("oldest", "account-a")
	first.Set("newer", "account-b")
	first.GetAndRefresh("oldest")
	first.Set("newest", "account-c")
	first.mu.Lock()
	entry := first.entries["oldest"]
	first.replaceAliasGroupsLocked(entry.authID, time.Now().Add(-time.Minute), entry.aliases, entry.binding, entry)
	first.persistence.Flush()
	first.mu.Unlock()
	restored := newSessionCache(time.Minute, 2, true)
	t.Cleanup(restored.Stop)
	for _, key := range []string{"oldest", "newer"} {
		if got, ok := restored.Get(key); ok {
			t.Fatalf("expired/evicted %q restored as %q", key, got)
		}
	}
	if got, ok := restored.Get("newest"); !ok || got != "account-c" {
		t.Fatalf("live binding not restored: %q, %v", got, ok)
	}
	if expiry := restored.entries["newest"].expiresAt; expiry.After(time.Now().Add(time.Minute)) {
		t.Fatalf("restore ignored shorter TTL: %v", expiry)
	}
}
