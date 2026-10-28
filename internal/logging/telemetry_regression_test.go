package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/outbox"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

func TestTelemetryAttemptSpoolsLazily(t *testing.T) {
	dir := t.TempDir()
	capture := NewTelemetryCapture(dir)
	capture.StartAttempt(record.Attempt{})
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty attempt created files: %v %v", entries, err)
	}
	capture.Chunk([]byte("answer"))
	capture.Timeline([]byte("frame"))
	got := capture.Finish()
	if string(got[0].Response) != "answer" || string(got[0].WebsocketTimeline) != "frame" {
		t.Fatal(got)
	}
	entries, _ = os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("spools not cleaned")
	}
}
func TestRenderedRequestErrors(t *testing.T) {
	got := RenderRequestLog(&record.Request{StartedAt: time.Now(), Errors: []string{"HTTP Status: 403\nlegacy failure"}, Status: 403})
	if !strings.Contains(string(got), "legacy failure") {
		t.Fatal(string(got))
	}
}
func TestTelemetryCompatibilityMethodsDoNotPublish(t *testing.T) {
	dir := t.TempDir()
	l := NewTelemetryRequestLogger(true, dir, "")
	if err := l.LogRequest("/v1/messages", "POST", nil, nil, 200, nil, nil, nil, nil, nil, nil, nil, "compat", time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	writer, err := l.LogStreamingRequest("/v1/messages", "POST", nil, nil, "compat-stream")
	if err != nil {
		t.Fatal(err)
	}
	writer.WriteChunkAsync([]byte("ok"))
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := outbox.List(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("compatibility paths emitted records: %v %v", files, err)
	}
}

func TestTelemetryStartupReclaimsOnlyStaleCaptureTemps(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "telemetry-outbox")
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(folder, ".capture-old.tmp")
	fresh := filepath.Join(folder, ".capture-fresh.tmp")
	for _, path := range []string{old, fresh} {
		if err := os.WriteFile(path, []byte("unfinished"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, at, at); err != nil {
		t.Fatal(err)
	}
	NewTelemetryRequestLogger(true, dir, "")
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("stale temp retained: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("active temp removed: %v", err)
	}
}

func TestOutboxEvictionWarnsWithLostBytes(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "telemetry-outbox")
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "old.json.zst"), []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	hook := test.NewLocal(log.StandardLogger())
	defer hook.Reset()
	if _, err := enforceLogDirSizeLimit(dir, 1, ""); err != nil {
		t.Fatal(err)
	}
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel && entry.Data["files"] == 1 && entry.Data["bytes"] == int64(10) {
			return
		}
	}
	t.Fatalf("missing warning with lost files/bytes: %+v", hook.AllEntries())
}

func TestOutboxEvictionWarningRendersLostBytes(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "telemetry-outbox")
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "old.json.zst"), []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	hook := test.NewLocal(log.StandardLogger())
	defer hook.Reset()
	if _, err := enforceLogDirSizeLimit(dir, 1, ""); err != nil {
		t.Fatal(err)
	}
	for _, entry := range hook.AllEntries() {
		if entry.Level != log.WarnLevel {
			continue
		}
		rendered, err := (&LogFormatter{}).Format(entry)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(rendered), "1 file") && strings.Contains(string(rendered), "10 bytes") {
			return
		}
	}
	t.Fatal("production formatter hid eviction counts and bytes")
}
