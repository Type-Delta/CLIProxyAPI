package api

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestUsageLimitSnapshotPathIsNotDirectlyInAuthDir keeps the usage snapshot out
// of the credential namespace. Every *.json placed DIRECTLY in auth-dir is
// treated as credential material: the watcher fires an auth reload for it
// (internal/watcher/events.go, isAuthJSON) and the auth loader picks it up via
// ReadDir. Storing the snapshot there causes an auth reload on every flush and
// leaves a non-credential file where unrecognised entries may later be pruned.
//
// Both the fsnotify watch and the auth directory scan are non-recursive, so the
// snapshot must live in a SUBDIRECTORY of auth-dir. Do not flatten this path.
func TestUsageLimitSnapshotPathIsNotDirectlyInAuthDir(t *testing.T) {
	tests := []struct {
		name    string
		authDir string
	}{
		{name: "unix style", authDir: filepath.Clean("/var/lib/cli-proxy/auths")},
		{name: "relative", authDir: filepath.Clean("auths")},
		{name: "temp dir", authDir: t.TempDir()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := usageLimitSnapshotPath(test.authDir)

			if filepath.Dir(path) == filepath.Clean(test.authDir) {
				t.Fatalf("snapshot %q sits directly in auth-dir; the auth watcher reloads on it", path)
			}
			if !strings.HasSuffix(path, ".json") {
				t.Fatalf("snapshot %q should be a .json file", path)
			}
			if base := filepath.Base(path); base != "usage-limits.json" {
				t.Fatalf("snapshot file name = %q, want usage-limits.json", base)
			}
			if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(test.authDir)) {
				t.Fatalf("snapshot %q escaped auth-dir %q", path, test.authDir)
			}
		})
	}
}

// TestUsageLimitSnapshotPathIsDeterministic guards the restore path: a differing
// path between runs would silently discard persisted counters on restart.
func TestUsageLimitSnapshotPathIsDeterministic(t *testing.T) {
	authDir := t.TempDir()
	if first, second := usageLimitSnapshotPath(authDir), usageLimitSnapshotPath(authDir); first != second {
		t.Fatalf("path is not stable: %q vs %q", first, second)
	}
}
