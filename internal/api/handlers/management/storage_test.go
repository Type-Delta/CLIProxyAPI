package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/storageusage"
)

func TestStorageUsageUsesManagedLocations(t *testing.T) {
	root := t.TempDir()
	logs := filepath.Join(root, "logs")
	auths := filepath.Join(root, "auths")
	for _, dir := range []string{logs, auths} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(logs, "main.log"), make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(auths, "credential.json"), []byte(`{"secret":"do-not-return"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{cfg: &config.Config{AuthDir: auths}, logDir: logs, configFilePath: filepath.Join(root, "config.yaml")}
	handler.cfg.Plugins.Dir = filepath.Join(root, "plugins")
	ctx, response := v2Request(http.MethodGet, "/v8/management/storage?path=/", "")
	handler.GetStorageUsage(ctx)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v", response.Code, response.Header())
	}
	var snapshot storageusage.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Partial || len(snapshot.Volumes) == 0 || snapshot.Volumes[0].LogsBytes < 8192 || snapshot.Volumes[0].DataBytes == 0 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	for _, volume := range snapshot.Volumes {
		for _, location := range volume.Locations {
			if location.Path == "/" {
				t.Fatal("client-supplied path was scanned")
			}
		}
	}
	if bytes.Contains(response.Body.Bytes(), []byte("do-not-return")) {
		t.Fatal("file content leaked in storage response")
	}
}

type storagePathService struct {
	cpauk.Service
	path string
}

func (s storagePathService) LocalStoragePath() string { return s.path }

func TestStorageUsageMeasuresActiveDatabaseWithoutClaimingSharedParent(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(shared, "analytics.db")
	for _, path := range []string{database, database + "-wal", filepath.Join(shared, "unrelated-app.db")} {
		if err := os.WriteFile(path, make([]byte, 8192), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	handler := &Handler{
		cfg:    &config.Config{AuthDir: filepath.Join(root, "auths")},
		logDir: filepath.Join(root, "logs"), configFilePath: filepath.Join(root, "config.yaml"),
		analytics: storagePathService{path: database},
	}
	handler.cfg.Plugins.Dir = filepath.Join(root, "plugins")
	handler.cfg.Analytics.Path = filepath.Join(root, "pending-restart", "analytics.db")
	ctx, response := v2Request(http.MethodGet, "/v8/management/storage", "")
	handler.GetStorageUsage(ctx)
	var snapshot storageusage.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	var expectedMeter storageusage.Meter
	expected := expectedMeter.Measure(ctx.Request.Context(), []storageusage.Location{{Path: database, Category: "data"}, {Path: database + "-wal", Category: "data"}})
	if snapshot.Partial || len(snapshot.Volumes) != 1 || snapshot.Volumes[0].DataBytes != expected.Volumes[0].DataBytes {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	for _, location := range snapshot.Volumes[0].Locations {
		if location.Path == shared || location.Path == handler.cfg.Analytics.Path {
			t.Fatalf("incorrectly attributed %s to active CPA data", location.Path)
		}
	}
}

func TestStorageUsageKeepsLocalVolumesWhenClickHouseIsUnavailable(t *testing.T) {
	root := t.TempDir()
	logs := filepath.Join(root, "logs")
	auths := filepath.Join(root, "auths")
	for _, dir := range []string{logs, auths} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(logs, "main.log"), make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: auths}
	cfg.ClickHouse.DSN = "clickhouse://127.0.0.1:1"
	cfg.ClickHouse.Database = "cpa_unreachable"
	handler := &Handler{cfg: cfg, logDir: logs, configFilePath: filepath.Join(root, "config.yaml")}
	handler.SetClickHouse(clickhouse.NewHandle(clickhouse.Config{DSN: cfg.ClickHouse.DSN, Database: cfg.ClickHouse.Database}))
	ctx, response := v2Request(http.MethodGet, "/v8/management/storage", "")
	handler.GetStorageUsage(ctx)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var snapshot storageusage.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ClickHouse.Reachable || len(snapshot.ClickHouse.Disks) != 0 {
		t.Fatalf("clickhouse = %+v", snapshot.ClickHouse)
	}
	if snapshot.Partial || len(snapshot.Volumes) == 0 || snapshot.Volumes[0].LogsBytes < 8192 {
		t.Fatalf("local snapshot = %+v", snapshot)
	}
}

func TestAuditStorageIncludesCustomKVDatabase(t *testing.T) {
	root := t.TempDir()
	database := filepath.Join(root, "external-kv", "kv.db")
	if err := os.MkdirAll(filepath.Dir(database), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(database, make([]byte, 8*1024*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{cfg: &config.Config{AuthDir: filepath.Join(root, "auths")}, logDir: filepath.Join(root, "logs"), configFilePath: filepath.Join(root, "config.yaml")}
	handler.cfg.Plugins.Dir = filepath.Join(root, "plugins")
	handler.cfg.KVStore.Path = database
	ctx, response := v2Request(http.MethodGet, "/v8/management/storage", "")
	handler.GetStorageUsage(ctx)
	var snapshot storageusage.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	var meter storageusage.Meter
	expected := meter.Measure(ctx.Request.Context(), []storageusage.Location{{Path: database, Category: "data"}})
	var got, want uint64
	for _, v := range snapshot.Volumes {
		got += v.DataBytes
	}
	for _, v := range expected.Volumes {
		want += v.DataBytes
	}
	t.Logf("data bytes=%d, custom KV allocation=%d", got, want)
	if got < want {
		t.Fatalf("configured KV database omitted: data=%d, KV=%d", got, want)
	}
}

func TestStorageUsageKVFilesAndModeChanges(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(shared, "kv.db")
	nextDatabase := filepath.Join(shared, "next.db")
	for _, path := range []string{database, database + "-wal", database + ".backup", nextDatabase, filepath.Join(shared, "unrelated.db")} {
		if err := os.WriteFile(path, make([]byte, 8192), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A same-prefix directory must never be claimed as a database sidecar.
	if err := os.Mkdir(database+".other-app", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(database+".other-app", "data"), make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	// Duplicate allocation via a sidecar and a scanned root must count once.
	auths := filepath.Join(root, "auths")
	if err := os.Mkdir(auths, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(auths, "kv.db"), make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(database, filepath.Join(auths, "kv-link")); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{cfg: &config.Config{AuthDir: auths}, logDir: filepath.Join(root, "logs"), configFilePath: filepath.Join(root, "config.yaml")}
	handler.cfg.Plugins.Dir = filepath.Join(root, "plugins")
	for _, tc := range []struct {
		name, path     string
		disabled, home bool
		files          []string
	}{
		{name: "external with sidecars", path: database, files: []string{database, database + "-wal", database + ".backup"}},
		{name: "changed path invalidates cache", path: nextDatabase, files: []string{nextDatabase}},
		{name: "disabled invalidates cache", path: nextDatabase, disabled: true},
		{name: "Home skips local KV", path: database, home: true},
		{name: "default overlaps auth root", path: "", files: []string{filepath.Join(auths, "kv.db")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler.cfg.KVStore.Path = tc.path
			handler.cfg.KVStore.Disabled = tc.disabled
			handler.cfg.Home.Enabled = tc.home
			ctx, response := v2Request(http.MethodGet, "/v8/management/storage", "")
			handler.GetStorageUsage(ctx)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var got storageusage.Snapshot
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			locations := []storageusage.Location{{Path: auths, Category: "data"}}
			for _, path := range tc.files {
				locations = append(locations, storageusage.Location{Path: path, Category: "data"})
			}
			var meter storageusage.Meter
			expected := meter.Measure(ctx.Request.Context(), locations)
			if got.Partial || len(got.Volumes) != 1 || got.Volumes[0].DataBytes != expected.Volumes[0].DataBytes {
				t.Fatalf("snapshot=%+v; expected=%+v", got, expected)
			}
			for _, location := range got.Volumes[0].Locations {
				if location.Path == shared || location.Path == database+".other-app" || location.Path == filepath.Join(shared, "unrelated.db") {
					t.Fatalf("claimed unrelated location: %s", location.Path)
				}
				if (tc.disabled || tc.home) && (location.Path == database || location.Path == nextDatabase) {
					t.Fatalf("inactive local KV claimed: %s", location.Path)
				}
			}
		})
	}
}
