package logging

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnforceLogDirSizeLimitDeletesOldest(t *testing.T) {
	dir := t.TempDir()

	writeLogFile(t, filepath.Join(dir, "old.log"), 60, time.Unix(1, 0))
	writeLogFile(t, filepath.Join(dir, "mid.log"), 60, time.Unix(2, 0))
	protected := filepath.Join(dir, "main.log")
	writeLogFile(t, protected, 60, time.Unix(3, 0))

	deleted, err := enforceLogDirSizeLimit(dir, 120, protected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 deleted file, got %d", deleted)
	}

	if _, err := os.Stat(filepath.Join(dir, "old.log")); !os.IsNotExist(err) {
		t.Fatalf("expected old.log to be removed, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mid.log")); err != nil {
		t.Fatalf("expected mid.log to remain, stat error: %v", err)
	}
	if _, err := os.Stat(protected); err != nil {
		t.Fatalf("expected protected main.log to remain, stat error: %v", err)
	}
}

func TestEnforceLogDirSizeLimitSkipsProtected(t *testing.T) {
	dir := t.TempDir()

	protected := filepath.Join(dir, "main.log")
	writeLogFile(t, protected, 200, time.Unix(1, 0))
	writeLogFile(t, filepath.Join(dir, "other.log"), 50, time.Unix(2, 0))

	deleted, err := enforceLogDirSizeLimit(dir, 100, protected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 deleted file, got %d", deleted)
	}

	if _, err := os.Stat(protected); err != nil {
		t.Fatalf("expected protected main.log to remain, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "other.log")); !os.IsNotExist(err) {
		t.Fatalf("expected other.log to be removed, stat error: %v", err)
	}
}

func writeLogFile(t *testing.T, path string, size int, modTime time.Time) {
	t.Helper()

	data := make([]byte, size)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("set times: %v", err)
	}
}

func TestLogDirCleanerIncludesOutbox(t *testing.T) {
	dir := t.TempDir()
	outboxDir := filepath.Join(dir, "telemetry-outbox")
	if err := os.Mkdir(outboxDir, 0700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(outboxDir, "1-id.json.zst")
	if err := os.WriteFile(old, []byte("123456"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	newer := filepath.Join(dir, "request.log")
	if err := os.WriteFile(newer, []byte("123456"), 0600); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(outboxDir, "writing.tmp")
	if err := os.WriteFile(tmp, []byte("in progress"), 0600); err != nil {
		t.Fatal(err)
	}
	deleted, err := enforceLogDirSizeLimit(dir, 6, "")
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("outbox record remains: %v", err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("temporary record removed: %v", err)
	}
}
