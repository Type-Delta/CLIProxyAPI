package storageusage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
}

func allocation(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, size := fileUsage(path, info)
	return size
}

func testDisk(path string) (disk, error) {
	info, err := os.Stat(path)
	if err != nil {
		return disk{}, err
	}
	id, _, _ := fileUsage(path, info)
	return disk{id: id, path: string(filepath.Separator), capacity: 1 << 30, available: 1 << 29}, nil
}

func TestStorageSeparatesLogsAndDataWithoutDoubleCounting(t *testing.T) {
	root := t.TempDir()
	logs := filepath.Join(root, "logs")
	writeFile(t, filepath.Join(logs, "main.log"))
	writeFile(t, filepath.Join(root, "analytics.db"))
	writeFile(t, filepath.Join(root, "backup.db"))
	result := measure(context.Background(), []Location{
		{logs, "logs"}, {root, "data"}, {filepath.Join(root, "analytics.db"), "data"},
	}, testDisk)
	if result.Partial || len(result.Volumes) != 1 {
		t.Fatalf("snapshot = %+v", result)
	}
	volume := result.Volumes[0]
	wantLogs := allocation(t, logs) + allocation(t, filepath.Join(logs, "main.log"))
	wantData := allocation(t, root) + allocation(t, filepath.Join(root, "analytics.db")) + allocation(t, filepath.Join(root, "backup.db"))
	if volume.LogsBytes != wantLogs || volume.DataBytes != wantData {
		t.Fatalf("logs/data = %d/%d, want %d/%d", volume.LogsBytes, volume.DataBytes, wantLogs, wantData)
	}
	if volume.LogsBytes+volume.DataBytes+volume.OtherBytes+volume.AvailableBytes != volume.CapacityBytes {
		t.Fatalf("inconsistent partition: %+v", volume)
	}
}

func TestStorageHardLinksSparseFilesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	writeFile(t, filepath.Join(root, "original"))
	writeFile(t, filepath.Join(external, "unrelated"))
	if err := os.Link(filepath.Join(root, "original"), filepath.Join(root, "linked")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := os.Symlink(external, filepath.Join(root, "external")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	sparse := filepath.Join(root, "sparse")
	writeFile(t, sparse)
	if err := os.Truncate(sparse, 64<<20); err != nil {
		t.Fatal(err)
	}
	result := measure(context.Background(), []Location{{root, "data"}}, testDisk)
	want := allocation(t, root) + allocation(t, filepath.Join(root, "original")) + allocation(t, filepath.Join(root, "external")) + allocation(t, sparse)
	if result.Partial || result.Volumes[0].DataBytes != want {
		t.Fatalf("snapshot = %+v, want data %d", result, want)
	}
	if runtime.GOOS != "windows" && result.Volumes[0].DataBytes >= 64<<20 {
		t.Fatal("sparse logical size was counted instead of allocated storage")
	}
}

func TestStorageMissingRootsStillReportFilesystem(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-created", "analytics")
	result := measure(context.Background(), []Location{{root, "data"}}, inspectVolume)
	if result.Partial || len(result.Volumes) != 1 || result.Volumes[0].CapacityBytes == 0 || result.Volumes[0].DataBytes != 0 {
		t.Fatalf("snapshot = %+v", result)
	}
}

func TestStorageSeparateFilesystemsAndUnavailableLocations(t *testing.T) {
	root := t.TempDir()
	logs, data := filepath.Join(root, "logs"), filepath.Join(root, "data")
	writeFile(t, filepath.Join(logs, "main.log"))
	writeFile(t, filepath.Join(data, "analytics.db"))
	result := measure(context.Background(), []Location{{logs, "logs"}, {data, "data"}, {filepath.Join(root, "unavailable"), "data"}}, func(path string) (disk, error) {
		if within(path, logs) {
			return disk{id: "logs-drive", path: logs, capacity: 1 << 30, available: 1 << 29}, nil
		}
		if within(path, data) {
			return disk{id: "data-drive", path: data, capacity: 2 << 30, available: 1 << 30}, nil
		}
		return disk{}, errors.New("unavailable filesystem")
	})
	if !result.Partial || len(result.Volumes) != 2 || result.Volumes[0].LogsBytes == 0 || result.Volumes[0].DataBytes != 0 || result.Volumes[1].DataBytes == 0 || result.Volumes[1].LogsBytes != 0 {
		t.Fatalf("snapshot = %+v", result)
	}
}

func TestStorageCanceledScanIsPartial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := measure(ctx, []Location{{t.TempDir(), "data"}}, testDisk)
	if !result.Partial {
		t.Fatal("canceled scan reported as complete")
	}
}

func TestStorageCacheExpiresAndTracksLocationChanges(t *testing.T) {
	root := t.TempDir()
	locations := []Location{{root, "data"}}
	var meter Meter
	initial := meter.Measure(context.Background(), locations)
	writeFile(t, filepath.Join(root, "new.db"))
	cached := meter.Measure(context.Background(), locations)
	if cached.MeasuredAt != initial.MeasuredAt || cached.Volumes[0].DataBytes != initial.Volumes[0].DataBytes {
		t.Fatal("cache did not reuse its snapshot")
	}
	meter.snapshot.MeasuredAt = meter.snapshot.MeasuredAt.Add(-time.Minute)
	updated := meter.Measure(context.Background(), locations)
	if updated.Volumes[0].DataBytes <= initial.Volumes[0].DataBytes {
		t.Fatal("expired cache did not rescan files")
	}
	changed := meter.Measure(context.Background(), []Location{{root, "logs"}})
	if changed.Volumes[0].DataBytes != 0 || changed.Volumes[0].LogsBytes == 0 {
		t.Fatal("changed locations reused the old cache")
	}
}
