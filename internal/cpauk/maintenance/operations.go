package maintenance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/importer"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
)

var ErrBackupInvalid = errors.New("analytics backup is invalid")

type Store interface {
	IntegrityCheck(context.Context) error
	Checkpoint(context.Context) error
	Reindex(context.Context) error
	Backup(context.Context, string) (store.BackupManifest, error)
	Restore(context.Context, string, string) error
	PreviewPurgeByKeyID(context.Context, string) (int64, error)
	PurgeByKeyID(context.Context, string) (int64, error)
	RollbackImport(context.Context, string) (int64, error)
	ApplyRetention(context.Context, time.Time, int) (store.RetentionResult, error)
	ApplyRetentionPolicy(context.Context, time.Time, time.Time, int) (store.RetentionResult, error)
	Reprice(context.Context, store.RepriceOptions, func(int, string)) (store.RepriceResult, error)
	IdentityKeyArray() [32]byte
	WriteImportBatch(context.Context, []model.Event, string) (int64, error)
	LoadImportCheckpoint(context.Context, string) ([]byte, bool, error)
	SaveImportCheckpoint(context.Context, string, string, string, int64, int, []byte, bool, [5]int64) error
	StartNewIdentityEpoch(context.Context) (store.EpochResult, error)
}

func StoreOperations(database Store) map[string]Operation {
	return map[string]Operation{
		"reprice": func(ctx context.Context, options map[string]any, progress ProgressFunc) (map[string]any, error) {
			selected, ok := options["range"].(model.Range)
			if !ok {
				return nil, fmt.Errorf("maintenance option range is required")
			}
			dryRun, err := boolOption(options, "dry_run", false)
			if err != nil {
				return nil, err
			}
			resume, err := boolOption(options, "resume", false)
			if err != nil {
				return nil, err
			}
			chunkSize, err := intOption(options, "chunk_size", 500, 1, 10_000)
			if err != nil {
				return nil, err
			}
			checkpoint, _ := options["checkpoint"].(string)
			var matched, updated int64
			var effectiveStart time.Time
			var retainedCutoff *time.Time
			historyComplete := true
			for {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				result, errReprice := database.Reprice(ctx, store.RepriceOptions{
					Range: selected, DryRun: dryRun, Resume: resume,
					ChunkSize: chunkSize, ResumeCheckpoint: checkpoint,
				}, nil)
				if errReprice != nil {
					return nil, errReprice
				}
				matched = result.Matched
				updated += result.Updated
				checkpoint = result.Checkpoint
				effectiveStart = result.EffectiveStart
				retainedCutoff = result.RetainedCutoff
				historyComplete = result.HistoryComplete
				percent := 95
				if matched > 0 && !dryRun {
					percent = min(95, int(updated*95/matched))
				}
				progress(percent, checkpoint)
				if result.Completed {
					return map[string]any{
						"matched": matched, "updated": updated, "checkpoint": checkpoint,
						"completed": true, "dry_run": dryRun, "effective_start": effectiveStart,
						"retained_cutoff": retainedCutoff, "history_complete": historyComplete,
					}, nil
				}
				resume = false
			}
		},
		"integrity_check": func(ctx context.Context, _ map[string]any, progress ProgressFunc) (map[string]any, error) {
			progress(50, "integrity_check")
			if err := database.IntegrityCheck(ctx); err != nil {
				return nil, err
			}
			return map[string]any{"integrity": "ok"}, nil
		},
		"checkpoint": func(ctx context.Context, _ map[string]any, progress ProgressFunc) (map[string]any, error) {
			progress(50, "wal_checkpoint")
			return map[string]any{"checkpoint": "complete"}, database.Checkpoint(ctx)
		},
		"reindex": func(ctx context.Context, _ map[string]any, progress ProgressFunc) (map[string]any, error) {
			progress(50, "reindex")
			return map[string]any{"reindex": "complete"}, database.Reindex(ctx)
		},
		"backup": func(ctx context.Context, options map[string]any, progress ProgressFunc) (map[string]any, error) {
			path, err := stringOption(options, "path")
			if err != nil {
				return nil, err
			}
			progress(20, "online_backup")
			manifest, err := database.Backup(ctx, path)
			if err != nil {
				return nil, err
			}
			backupID, err := store.BackupID(manifest)
			if err != nil {
				return nil, err
			}
			progress(90, "verify_backup")
			return map[string]any{"backup_id": backupID, "database_sha256": manifest.DatabaseSHA256, "identity_epoch": manifest.IdentityEpoch}, nil
		},
		"restore": func(ctx context.Context, options map[string]any, progress ProgressFunc) (map[string]any, error) {
			path, err := stringOption(options, "path")
			if err != nil {
				return nil, err
			}
			manifest, err := stringOption(options, "manifest")
			if err != nil {
				return nil, err
			}
			backupID, err := stringOption(options, "backup_id")
			if err != nil {
				return nil, err
			}
			progress(20, "verify_backup")
			verified, err := store.VerifyBackup(ctx, path, manifest)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrBackupInvalid, err)
			}
			expectedID, err := store.BackupID(verified)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrBackupInvalid, err)
			}
			if backupID != expectedID {
				return nil, fmt.Errorf("%w: backup ID does not match the verified manifest", ErrBackupInvalid)
			}
			if err := database.Restore(ctx, path, manifest); err != nil {
				return nil, err
			}
			return map[string]any{"backup_id": backupID, "restored": true}, nil
		},
		"purge_key": func(ctx context.Context, options map[string]any, progress ProgressFunc) (map[string]any, error) {
			keyID, err := stringOption(options, "key_id")
			if err != nil {
				return nil, err
			}
			previewRows, err := database.PreviewPurgeByKeyID(ctx, keyID)
			if err != nil {
				return nil, err
			}
			batchID := purgePreviewBatchID(database.IdentityKeyArray(), keyID, previewRows)
			preview, _ := options["preview"].(bool)
			if preview {
				return map[string]any{"batch_id": batchID, "preview": true, "rows": previewRows}, nil
			}
			confirmed, _ := options["confirmed"].(bool)
			providedBatchID, err := stringOption(options, "batch_id")
			if !confirmed || err != nil || !hmac.Equal([]byte(providedBatchID), []byte(batchID)) {
				return nil, fmt.Errorf("purge requires a matching preview batch ID and explicit confirmation")
			}
			backupPath, err := stringOption(options, "backup_path")
			if err != nil {
				return nil, fmt.Errorf("purge requires a backup path: %w", err)
			}
			progress(20, "backup")
			manifest, err := database.Backup(ctx, backupPath)
			if err != nil {
				return nil, err
			}
			backupID, err := store.BackupID(manifest)
			if err != nil {
				return nil, err
			}
			progress(60, "purge")
			removed, err := database.PurgeByKeyID(ctx, keyID)
			return map[string]any{"batch_id": batchID, "backup_id": backupID, "removed_rows": removed}, err
		},
		"rollback_import": func(ctx context.Context, options map[string]any, progress ProgressFunc) (map[string]any, error) {
			batchID, err := stringOption(options, "batch_id")
			if err != nil {
				return nil, err
			}
			progress(50, "rollback_import")
			removed, err := database.RollbackImport(ctx, batchID)
			return map[string]any{"removed_rows": removed, "reconciled": err == nil}, err
		},
		"import_cpauk": func(ctx context.Context, options map[string]any, progress ProgressFunc) (map[string]any, error) {
			path, err := stringOption(options, "path")
			if err != nil {
				return nil, err
			}
			dryRun, err := boolOption(options, "dry_run", false)
			if err != nil {
				return nil, err
			}
			resume, err := boolOption(options, "resume", false)
			if err != nil {
				return nil, err
			}
			storeCredential, err := boolOption(options, "store_credential", false)
			if err != nil {
				return nil, err
			}
			rollbackOnFailure, err := boolOption(options, "rollback_on_failure", false)
			if err != nil {
				return nil, err
			}
			chunkSize, err := intOption(options, "chunk_size", 500, 1, 10_000)
			if err != nil {
				return nil, err
			}
			batchID, _ := options["batch_id"].(string)
			backupPath, _ := options["backup_path"].(string)
			progress(10, "open_source")
			source, err := importer.OpenCPAUKV115Source(ctx, path)
			if err != nil {
				return nil, err
			}
			worker := importer.Importer{Destination: database,
				Transform: importer.NewCPAUKV115Transformer(database.IdentityKeyArray(), storeCredential)}
			progress(20, "verified_backup")
			result, err := worker.RunWithBackup(ctx, source, importer.Options{
				BatchID: batchID, DryRun: dryRun, Resume: resume, ChunkSize: chunkSize,
			}, backupPath)
			if err != nil && rollbackOnFailure && result.BatchID != "" {
				if _, rollbackErr := database.RollbackImport(context.Background(), result.BatchID); rollbackErr != nil {
					return nil, fmt.Errorf("import failed: %v; rollback failed: %w", err, rollbackErr)
				}
			}
			if err != nil {
				return nil, err
			}
			progress(95, "reconcile")
			return map[string]any{"batch_id": result.BatchID, "dry_run": result.DryRun,
				"rows_read": result.RowsRead, "transformed": result.Transformed, "inserted": result.Inserted,
				"skipped": result.Skipped, "rejected": result.Rejected, "reconciled": result.Reconciled}, nil
		},
		"retention": func(ctx context.Context, options map[string]any, progress ProgressFunc) (map[string]any, error) {
			cutoffText, err := stringOption(options, "raw_cutoff")
			if err != nil {
				cutoffText, err = stringOption(options, "cutoff")
			}
			if err != nil {
				return nil, err
			}
			cutoff, err := time.Parse(time.RFC3339Nano, cutoffText)
			if err != nil {
				return nil, fmt.Errorf("parse retention cutoff: %w", err)
			}
			var hourlyCutoff time.Time
			if value, ok := options["hourly_cutoff"].(string); ok && value != "" {
				hourlyCutoff, err = time.Parse(time.RFC3339Nano, value)
				if err != nil {
					return nil, fmt.Errorf("parse hourly retention cutoff: %w", err)
				}
				hourlyCutoff = hourlyCutoff.UTC()
			}
			progress(10, "hourly_rollup")
			result, err := database.ApplyRetentionPolicy(ctx, cutoff.UTC(), hourlyCutoff, 1000)
			return map[string]any{"deleted_rows": result.DeletedRows, "rolled_up_rows": result.RolledUpRows,
				"daily_rolled_up_rows": result.DailyRolledUpRows, "deleted_hourly_rows": result.DeletedHourlyRows}, err
		},
		"repair": func(ctx context.Context, options map[string]any, progress ProgressFunc) (map[string]any, error) {
			action, err := stringOption(options, "action")
			if err != nil {
				return nil, err
			}
			progress(40, action)
			switch action {
			case "integrity_check":
				err = database.IntegrityCheck(ctx)
			case "checkpoint":
				err = database.Checkpoint(ctx)
			case "reindex":
				err = database.Reindex(ctx)
			default:
				return nil, fmt.Errorf("unsupported repair action %q", action)
			}
			return map[string]any{"action": action, "repaired": err == nil}, err
		},
		"start_new_identity_epoch": func(ctx context.Context, options map[string]any, progress ProgressFunc) (map[string]any, error) {
			confirmed, err := boolOption(options, "confirmed", false)
			if err != nil {
				return nil, err
			}
			if !confirmed {
				return nil, fmt.Errorf("identity epoch recovery requires explicit confirmation")
			}
			progress(20, "archive_identity_epoch")
			result, err := database.StartNewIdentityEpoch(ctx)
			if err != nil {
				return nil, err
			}
			return map[string]any{"identity_epoch": result.IdentityEpoch,
				"archived_database": result.ArchivedDB, "archived_identity_key": result.ArchivedKey}, nil
		},
	}
}

func purgePreviewBatchID(identityKey [32]byte, keyID string, rows int64) string {
	digest := hmac.New(sha256.New, identityKey[:])
	_, _ = digest.Write([]byte("cpa-analytics-purge-preview-v1\x00"))
	_, _ = digest.Write([]byte(keyID))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(strconv.FormatInt(rows, 10)))
	return "purge-" + hex.EncodeToString(digest.Sum(nil))
}

func stringOption(options map[string]any, name string) (string, error) {
	value, ok := options[name].(string)
	if !ok || value == "" {
		return "", fmt.Errorf("maintenance option %s is required", name)
	}
	return value, nil
}

func boolOption(options map[string]any, name string, fallback bool) (bool, error) {
	value, exists := options[name]
	if !exists {
		return fallback, nil
	}
	result, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("maintenance option %s must be boolean", name)
	}
	return result, nil
}

func intOption(options map[string]any, name string, fallback, minimum, maximum int) (int, error) {
	value, exists := options[name]
	if !exists {
		return fallback, nil
	}
	var result int
	switch typed := value.(type) {
	case int:
		result = typed
	case float64:
		result = int(typed)
		if float64(result) != typed {
			return 0, fmt.Errorf("maintenance option %s must be an integer", name)
		}
	default:
		return 0, fmt.Errorf("maintenance option %s must be an integer", name)
	}
	if result < minimum || result > maximum {
		return 0, fmt.Errorf("maintenance option %s must be between %d and %d", name, minimum, maximum)
	}
	return result, nil
}
