package maintenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
)

func TestControllerExcludesJobsAndCancelsAtOperationBoundary(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var attached atomic.Int64
	controller := New(Hooks{Attach: func(context.Context) error { attached.Add(1); return nil }}, map[string]Operation{
		"wait": func(ctx context.Context, _ map[string]any, _ ProgressFunc) (map[string]any, error) {
			close(started)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
				return nil, nil
			}
		},
	})
	status, err := controller.Start(context.Background(), Request{Kind: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := controller.Start(context.Background(), Request{Kind: "wait"}); err != ErrJobRunning {
		t.Fatalf("second job error = %v", err)
	}
	if err := controller.Cancel(context.Background(), status.JobID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status, err = controller.Status(context.Background(), status.JobID)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == model.JobCanceled {
			if attached.Load() != 1 {
				t.Fatalf("attach calls = %d", attached.Load())
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("job did not cancel")
}

func TestControllerAwaitShutdownAndPrune(t *testing.T) {
	started := make(chan struct{})
	controller := New(Hooks{}, map[string]Operation{
		"wait": func(ctx context.Context, _ map[string]any, _ ProgressFunc) (map[string]any, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
		"done": func(context.Context, map[string]any, ProgressFunc) (map[string]any, error) {
			return map[string]any{"ok": true}, nil
		},
	})
	waiting, err := controller.Start(context.Background(), Request{Kind: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
	status, err := controller.Await(context.Background(), waiting.JobID)
	if err != nil || status.State != model.JobCanceled {
		t.Fatalf("await status=%+v err=%v", status, err)
	}
	done, err := controller.Start(context.Background(), Request{Kind: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Await(context.Background(), done.JobID); err != nil {
		t.Fatal(err)
	}
	if removed := controller.Prune(1); removed != 1 {
		t.Fatalf("pruned jobs=%d", removed)
	}
	if _, err := controller.Status(context.Background(), waiting.JobID); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("old job status error=%v", err)
	}
	if err := controller.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Start(context.Background(), Request{Kind: "done"}); !errors.Is(err, ErrControllerClosed) {
		t.Fatalf("start after close error=%v", err)
	}
}

func TestStoreOperationsExposeImportRepairAndIdentityRecovery(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, store.Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	operations := StoreOperations(database)
	for _, name := range []string{"import_cpauk", "rollback_import", "repair", "start_new_identity_epoch"} {
		if operations[name] == nil {
			t.Fatalf("operation %q is missing", name)
		}
	}
	oldEpoch := database.IdentityEpoch()
	controller := New(Hooks{}, operations)
	job, err := controller.Start(ctx, Request{Kind: "start_new_identity_epoch", Options: map[string]any{"confirmed": true}})
	if err != nil {
		t.Fatal(err)
	}
	status, err := controller.Await(ctx, job.JobID)
	if err != nil || status.State != model.JobSucceeded {
		t.Fatalf("identity recovery status=%+v err=%v", status, err)
	}
	if database.IdentityEpoch() == oldEpoch {
		t.Fatal("identity epoch did not change")
	}
	archived, _ := status.Result["archived_database"].(string)
	if _, err := os.Stat(archived); err != nil {
		t.Fatalf("archived database: %v", err)
	}
}

func TestBackupAndRestoreBindVerifiedBackupID(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	database, err := store.Open(ctx, store.Config{Path: filepath.Join(directory, "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	operations := StoreOperations(database)
	backupPath := filepath.Join(directory, "backup.db")
	result, err := operations["backup"](ctx, map[string]any{"path": backupPath}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	backupID, ok := result["backup_id"].(string)
	if !ok || len(backupID) != len("backup-")+64 {
		t.Fatalf("backup ID=%q", backupID)
	}
	options := map[string]any{"path": backupPath, "manifest": backupPath + ".manifest.json", "backup_id": "backup-wrong"}
	if _, err := operations["restore"](ctx, options, func(int, string) {}); !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("restore mismatch error=%v", err)
	}
	controller := New(Hooks{}, operations)
	job, err := controller.Start(ctx, Request{Kind: "restore", Options: options})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := controller.Await(ctx, job.JobID)
	if err != nil || failed.State != model.JobFailed || failed.Error == nil || failed.Error.Code != model.ErrorAnalyticsBackupInvalid {
		t.Fatalf("invalid backup job=%+v err=%v", failed, err)
	}
	options["backup_id"] = backupID
	result, err = operations["restore"](ctx, options, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if result["backup_id"] != backupID || result["restored"] != true {
		t.Fatalf("restore result=%+v", result)
	}
}

func TestPurgeRequiresBoundPreviewConfirmationAndBackup(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	database, err := store.Open(ctx, store.Config{Path: filepath.Join(directory, "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	operation := StoreOperations(database)["purge_key"]
	keyID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	preview, err := operation(ctx, map[string]any{"key_id": keyID, "preview": true}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	batchID, _ := preview["batch_id"].(string)
	if len(batchID) != len("purge-")+64 || preview["preview"] != true || preview["rows"] != int64(0) {
		t.Fatalf("purge preview=%+v", preview)
	}
	backupPath := filepath.Join(directory, "purge-backup.db")
	if _, err = operation(ctx, map[string]any{
		"key_id": keyID, "confirmed": true, "batch_id": "purge-" + strings.Repeat("0", 64), "backup_path": backupPath,
	}, func(int, string) {}); err == nil {
		t.Fatal("purge accepted an unbound preview batch ID")
	}
	result, err := operation(ctx, map[string]any{
		"key_id": keyID, "confirmed": true, "batch_id": batchID, "backup_path": backupPath,
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if result["batch_id"] != batchID || result["removed_rows"] != int64(0) {
		t.Fatalf("purge result=%+v", result)
	}
	backupID, _ := result["backup_id"].(string)
	if len(backupID) != len("backup-")+64 {
		t.Fatalf("purge backup ID=%q", backupID)
	}
}

func TestControllerContainsOperationPanic(t *testing.T) {
	controller := New(Hooks{}, map[string]Operation{
		"panic": func(context.Context, map[string]any, ProgressFunc) (map[string]any, error) { panic("secret") },
	})
	status, err := controller.Start(context.Background(), Request{Kind: "panic"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status, err = controller.Status(context.Background(), status.JobID)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == model.JobFailed {
			if status.Error == nil || status.Error.Message == "secret" {
				t.Fatalf("unsafe panic result: %+v", status)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("panicking job did not fail")
}

func TestControllerCloseCancelsActiveJobBeforeReturning(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	controller := New(Hooks{}, map[string]Operation{
		"wait": func(ctx context.Context, _ map[string]any, _ ProgressFunc) (map[string]any, error) {
			close(started)
			<-ctx.Done()
			close(finished)
			return nil, ctx.Err()
		},
	})
	if _, err := controller.Start(context.Background(), Request{Kind: "wait"}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := controller.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("controller close returned before operation cancellation boundary")
	}
}
