package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
)

type backupDriver interface {
	NewBackup(string) (*sqlite.Backup, error)
	NewRestore(string) (*sqlite.Backup, error)
}

type BackupManifest struct {
	SchemaVersion          int    `json:"schema_version"`
	IdentityEpoch          string `json:"identity_epoch"`
	DatabaseFile           string `json:"database_file"`
	DatabaseSHA256         string `json:"database_sha256"`
	IdentityKeyFile        string `json:"identity_key_file"`
	IdentityKeySHA256      string `json:"identity_key_sha256"`
	IdentityKeyFingerprint string `json:"identity_key_fingerprint"`
	CreatedAt              string `json:"created_at"`
}

type RetentionResult struct {
	Cutoff            time.Time
	HourlyCutoff      time.Time
	RolledUpRows      int64
	DeletedRows       int64
	DailyRolledUpRows int64
	DeletedHourlyRows int64
	RawCheckpoint     *time.Time
	HourlyCheckpoint  *time.Time
}

func BackupID(manifest BackupManifest) (string, error) {
	digest, err := hex.DecodeString(manifest.DatabaseSHA256)
	if err != nil || len(digest) != sha256.Size {
		return "", fmt.Errorf("backup manifest database digest is invalid")
	}
	return "backup-" + manifest.DatabaseSHA256, nil
}

func (s *SQLiteStore) IntegrityCheck(ctx context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return ErrClosed
	}
	return s.integrityCheck(ctx)
}

func (s *SQLiteStore) integrityCheck(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("run analytics integrity check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return fmt.Errorf("scan analytics integrity result: %w", err)
		}
		if result != "ok" {
			return fmt.Errorf("%w: %s", ErrCorruptDatabase, result)
		}
	}
	return rows.Err()
}

func (s *SQLiteStore) Checkpoint(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("checkpoint analytics WAL: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Reindex(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	if _, err := s.db.ExecContext(ctx, "REINDEX"); err != nil {
		return fmt.Errorf("reindex analytics database: %w", err)
	}
	return s.integrityCheck(ctx)
}

func (s *SQLiteStore) Backup(ctx context.Context, destination string) (BackupManifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return BackupManifest{}, ErrClosed
	}
	if err := s.checkQuota(s.databaseSize()); err != nil {
		return BackupManifest{}, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return BackupManifest{}, fmt.Errorf("create backup directory: %w", err)
	}
	if _, err := os.Stat(destination); err == nil {
		return BackupManifest{}, fmt.Errorf("backup destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return BackupManifest{}, fmt.Errorf("inspect backup destination: %w", err)
	}
	if err := s.backupDatabase(ctx, destination); err != nil {
		return BackupManifest{}, err
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		return BackupManifest{}, fmt.Errorf("set backup permissions: %w", err)
	}
	identityDestination := destination + ".identity.key"
	if err := writeExclusive(identityDestination, s.identityKey, 0o600); err != nil {
		return BackupManifest{}, fmt.Errorf("write backup identity key: %w", err)
	}
	databaseChecksum, err := fileSHA256(destination)
	if err != nil {
		return BackupManifest{}, err
	}
	identityChecksum, err := fileSHA256(identityDestination)
	if err != nil {
		return BackupManifest{}, err
	}
	fingerprint, err := modelIdentityFingerprint(s.identityKey)
	if err != nil {
		return BackupManifest{}, err
	}
	manifest := BackupManifest{
		SchemaVersion: s.currentSchema, IdentityEpoch: s.identityEpoch,
		DatabaseFile: filepath.Base(destination), DatabaseSHA256: databaseChecksum,
		IdentityKeyFile: filepath.Base(identityDestination), IdentityKeySHA256: identityChecksum,
		IdentityKeyFingerprint: fingerprint, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return BackupManifest{}, fmt.Errorf("encode backup manifest: %w", err)
	}
	data = append(data, '\n')
	if err := writeExclusive(destination+".manifest.json", data, 0o600); err != nil {
		return BackupManifest{}, fmt.Errorf("write backup manifest: %w", err)
	}
	return manifest, nil
}

func (s *SQLiteStore) backupDatabase(ctx context.Context, destination string) error {
	connection, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire analytics backup connection: %w", err)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.Raw(func(driverConnection any) error {
		backuper, ok := driverConnection.(backupDriver)
		if !ok {
			return fmt.Errorf("SQLite driver does not expose online backup")
		}
		backup, err := backuper.NewBackup(destination)
		if err != nil {
			return err
		}
		for more := true; more; {
			if err := ctx.Err(); err != nil {
				_ = backup.Finish()
				return err
			}
			more, err = backup.Step(128)
			if err != nil {
				_ = backup.Finish()
				return err
			}
		}
		return backup.Finish()
	}); err != nil {
		return fmt.Errorf("back up analytics database: %w", err)
	}
	return verifyDatabase(ctx, destination)
}

func VerifyBackup(ctx context.Context, databasePath, manifestPath string) (BackupManifest, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("read backup manifest: %w", err)
	}
	var manifest BackupManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return BackupManifest{}, fmt.Errorf("decode backup manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return BackupManifest{}, fmt.Errorf("backup manifest contains trailing data")
	}
	if manifest.SchemaVersion < 1 || manifest.DatabaseFile != filepath.Base(databasePath) ||
		manifest.IdentityKeyFile != filepath.Base(databasePath)+".identity.key" ||
		filepath.Base(manifest.IdentityKeyFile) != manifest.IdentityKeyFile {
		return BackupManifest{}, fmt.Errorf("backup manifest file inventory is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, manifest.CreatedAt); err != nil {
		return BackupManifest{}, fmt.Errorf("backup manifest timestamp is invalid")
	}
	databaseChecksum, err := fileSHA256(databasePath)
	if err != nil {
		return BackupManifest{}, err
	}
	if databaseChecksum != manifest.DatabaseSHA256 {
		return BackupManifest{}, fmt.Errorf("backup database checksum mismatch")
	}
	identityPath := filepath.Join(filepath.Dir(manifestPath), manifest.IdentityKeyFile)
	identityChecksum, err := fileSHA256(identityPath)
	if err != nil {
		return BackupManifest{}, err
	}
	if identityChecksum != manifest.IdentityKeySHA256 {
		return BackupManifest{}, fmt.Errorf("backup identity checksum mismatch")
	}
	identity, err := os.ReadFile(identityPath)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("read backup identity key: %w", err)
	}
	fingerprint, err := modelIdentityFingerprint(identity)
	if err != nil || fingerprint != manifest.IdentityKeyFingerprint {
		return BackupManifest{}, fmt.Errorf("backup identity fingerprint mismatch")
	}
	if err := verifyDatabase(ctx, databasePath); err != nil {
		return BackupManifest{}, err
	}
	return manifest, nil
}

// Restore verifies the backup, materializes a temporary SQLite copy, then
// swaps files while intake is detached. The old database and identity key are
// retained with a rollback suffix.
func (s *SQLiteStore) Restore(ctx context.Context, databasePath, manifestPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	manifest, err := VerifyBackup(ctx, databasePath, manifestPath)
	if err != nil {
		return err
	}
	if manifest.SchemaVersion > s.currentSchema {
		return ErrUnsupportedSchema
	}
	if err := s.checkQuota(fileSize(databasePath) * 2); err != nil {
		return err
	}
	identityPath := filepath.Join(filepath.Dir(manifestPath), manifest.IdentityKeyFile)
	identity, err := os.ReadFile(identityPath)
	if err != nil {
		return fmt.Errorf("read backup identity key: %w", err)
	}
	fingerprint, err := modelIdentityFingerprint(identity)
	if err != nil || fingerprint != manifest.IdentityKeyFingerprint {
		return fmt.Errorf("backup identity fingerprint mismatch")
	}
	restoreID, err := newRestoreID()
	if err != nil {
		return err
	}
	temporaryDatabase := s.config.Path + ".restore-" + restoreID + ".tmp"
	temporaryIdentity := s.config.IdentityKeyPath + ".restore-" + restoreID + ".tmp"
	if err := copyDatabase(ctx, databasePath, temporaryDatabase); err != nil {
		return err
	}
	if err := writeExclusive(temporaryIdentity, identity, 0o600); err != nil {
		_ = os.Remove(temporaryDatabase)
		return fmt.Errorf("stage restored identity key: %w", err)
	}
	if err := verifyDatabase(ctx, temporaryDatabase); err != nil {
		_ = os.Remove(temporaryDatabase)
		_ = os.Remove(temporaryIdentity)
		return err
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		_ = os.Remove(temporaryDatabase)
		_ = os.Remove(temporaryIdentity)
		return fmt.Errorf("checkpoint before restore: %w", err)
	}
	oldDatabase := s.db
	s.db = nil
	if err := oldDatabase.Close(); err != nil {
		s.db = oldDatabase
		_ = os.Remove(temporaryDatabase)
		_ = os.Remove(temporaryIdentity)
		return fmt.Errorf("close analytics database for restore: %w", err)
	}
	rollbackDatabase := s.config.Path + ".rollback-" + restoreID
	rollbackIdentity := s.config.IdentityKeyPath + ".rollback-" + restoreID
	if err := os.Rename(s.config.Path, rollbackDatabase); err != nil {
		return fmt.Errorf("archive analytics database before restore: %w", err)
	}
	if err := os.Rename(s.config.IdentityKeyPath, rollbackIdentity); err != nil {
		_ = os.Rename(rollbackDatabase, s.config.Path)
		return fmt.Errorf("archive analytics identity key before restore: %w", err)
	}
	rollback := func() {
		_ = os.Remove(s.config.Path)
		_ = os.Remove(s.config.IdentityKeyPath)
		_ = os.Rename(rollbackDatabase, s.config.Path)
		_ = os.Rename(rollbackIdentity, s.config.IdentityKeyPath)
		database, openErr := openDatabase(context.Background(), s.config)
		if openErr == nil {
			s.db = database
		}
	}
	if err := os.Rename(temporaryDatabase, s.config.Path); err != nil {
		rollback()
		return fmt.Errorf("install restored analytics database: %w", err)
	}
	if err := os.Rename(temporaryIdentity, s.config.IdentityKeyPath); err != nil {
		rollback()
		return fmt.Errorf("install restored analytics identity key: %w", err)
	}
	database, err := openDatabase(ctx, s.config)
	if err != nil {
		rollback()
		return fmt.Errorf("open restored analytics database: %w", err)
	}
	candidate := &SQLiteStore{db: database, config: s.config, identityKey: identity}
	if err := candidate.initialize(ctx, false); err != nil {
		_ = database.Close()
		rollback()
		return fmt.Errorf("verify restored analytics database: %w", err)
	}
	s.db = candidate.db
	s.identityKey = candidate.identityKey
	s.identityEpoch = candidate.identityEpoch
	s.currentSchema = candidate.currentSchema
	s.retentionCutoff = candidate.retentionCutoff
	return nil
}

func (s *SQLiteStore) PurgeByKeyID(ctx context.Context, keyID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return 0, ErrClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin analytics key purge: %w", err)
	}
	var removed int64
	for _, statement := range []string{
		"DELETE FROM events WHERE key_id = ?",
		"DELETE FROM rollups WHERE key_id = ?",
		"DELETE FROM request_rollups WHERE key_id = ?",
	} {
		result, err := tx.ExecContext(ctx, statement, keyID)
		if err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("purge analytics key history: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		removed += count
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit analytics key purge: %w", err)
	}
	return removed, nil
}

func (s *SQLiteStore) PreviewPurgeByKeyID(ctx context.Context, keyID string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return 0, ErrClosed
	}
	var rows int64
	for _, statement := range []string{
		"SELECT COUNT(*) FROM events WHERE key_id = ?",
		"SELECT COUNT(*) FROM rollups WHERE key_id = ?",
		"SELECT COUNT(*) FROM request_rollups WHERE key_id = ?",
	} {
		var count int64
		if err := s.db.QueryRowContext(ctx, statement, keyID).Scan(&count); err != nil {
			return 0, fmt.Errorf("preview analytics key purge: %w", err)
		}
		rows += count
	}
	return rows, nil
}

func (s *SQLiteStore) RollbackImport(ctx context.Context, batchID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return 0, ErrClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin import rollback: %w", err)
	}
	var removed int64
	for _, statement := range []string{
		"DELETE FROM events WHERE import_batch_id = ?",
		"DELETE FROM rollups WHERE import_batch_id = ?",
		"DELETE FROM request_rollups WHERE import_batch_id = ?",
	} {
		result, err := tx.ExecContext(ctx, statement, batchID)
		if err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("remove imported rows: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		removed += count
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM import_checkpoints WHERE batch_id = ?", batchID); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("remove import checkpoint: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit import rollback: %w", err)
	}
	return removed, nil
}

func (s *SQLiteStore) ApplyRetention(ctx context.Context, cutoff time.Time, batchSize int) (RetentionResult, error) {
	return s.ApplyRetentionPolicy(ctx, cutoff, time.Time{}, batchSize)
}

func (s *SQLiteStore) ApplyRetentionPolicy(ctx context.Context, rawCutoff, hourlyCutoff time.Time, batchSize int) (RetentionResult, error) {
	if rawCutoff.IsZero() || rawCutoff.Location() != time.UTC || !hourlyCutoff.IsZero() && hourlyCutoff.Location() != time.UTC || batchSize < 1 || batchSize > 10_000 {
		return RetentionResult{}, fmt.Errorf("invalid retention cutoff or batch size")
	}
	if !hourlyCutoff.IsZero() && hourlyCutoff.After(rawCutoff) {
		return RetentionResult{}, fmt.Errorf("hourly retention cutoff must not follow raw cutoff")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return RetentionResult{}, ErrClosed
	}
	result := RetentionResult{Cutoff: rawCutoff, HourlyCutoff: hourlyCutoff}
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		rolled, deleted, checkpoint, err := s.rollRawBatch(ctx, rawCutoff, batchSize)
		if err != nil {
			return result, err
		}
		result.RolledUpRows += rolled
		result.DeletedRows += deleted
		if checkpoint != nil {
			result.RawCheckpoint = checkpoint
		}
		if deleted < int64(batchSize) {
			break
		}
	}
	if err := s.recordCompletedCutoff(ctx, "raw", rawCutoff); err != nil {
		return result, err
	}
	if !hourlyCutoff.IsZero() {
		for {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			rolled, deleted, checkpoint, err := s.rollHourlyBatch(ctx, hourlyCutoff, batchSize)
			if err != nil {
				return result, err
			}
			result.DailyRolledUpRows += rolled
			result.DeletedHourlyRows += deleted
			if checkpoint != nil {
				result.HourlyCheckpoint = checkpoint
			}
			if deleted == 0 {
				break
			}
		}
		if err := s.recordCompletedCutoff(ctx, "hourly", hourlyCutoff); err != nil {
			return result, err
		}
	}
	s.advanceEventRetentionFloor(rawCutoff)
	return result, s.integrityCheck(ctx)
}

const hourNanoseconds int64 = int64(time.Hour)
const dayNanoseconds int64 = int64(24 * time.Hour)
const rollupColumns = `grain,bucket_start_ns,bucket_end_ns,first_activity_ns,last_activity_ns,
key_id,provider,model,credential_id,endpoint_class,auth_type,service_tier,succeeded,error_class,
status_code,token_quality,latency_bucket,cache_class,import_batch_id,proxy_requests,upstream_attempts,input_tokens,
output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,
known_cost_nano,unpriced_tokens`
const requestRollupColumns = `grain,bucket_start_ns,bucket_end_ns,proxy_request_id,key_id,provider,
model,credential_id,endpoint_class,auth_type,service_tier,succeeded,error_class,status_code,
token_quality,latency_bucket,cache_class,import_batch_id`

func (s *SQLiteStore) rollRawBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, int64, *time.Time, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("begin raw retention batch: %w", err)
	}
	selector := `SELECT attempt_id FROM events WHERE ((requested_at_ns / ?) + 1) * ? <= ?
ORDER BY requested_at_ns, attempt_id LIMIT ?`
	selectorArgs := []any{hourNanoseconds, hourNanoseconds, cutoff.UnixNano(), batchSize}
	var checkpoint sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(requested_at_ns) FROM events WHERE attempt_id IN (`+selector+`)`, selectorArgs...).Scan(&checkpoint); err != nil {
		_ = tx.Rollback()
		return 0, 0, nil, fmt.Errorf("read raw retention checkpoint: %w", err)
	}
	rollupArgs := append([]any{hourNanoseconds, hourNanoseconds, hourNanoseconds, hourNanoseconds}, selectorArgs...)
	rollupArgs = append(rollupArgs, hourNanoseconds)
	rollupResult, err := tx.ExecContext(ctx, `INSERT INTO rollups (`+rollupColumns+`)
SELECT 'hourly', (requested_at_ns / ?) * ?, ((requested_at_ns / ?) + 1) * ?,
MIN(requested_at_ns), MAX(requested_at_ns),
key_id, provider, model, COALESCE(credential_id,''), endpoint_class, COALESCE(auth_type,''),
COALESCE(service_tier_used,service_tier_requested,''), succeeded, COALESCE(error_class,''),
COALESCE(upstream_status_code,0), token_quality,
CASE WHEN latency_ms < 100 THEN '<100ms' WHEN latency_ms < 500 THEN '100-499ms' WHEN latency_ms < 1000 THEN '500-999ms' ELSE '1000ms+' END,
CASE WHEN cached_tokens > 0 THEN 'cached' ELSE 'uncached' END,
COALESCE(import_batch_id,''),
COUNT(DISTINCT proxy_request_id), COUNT(*),
SUM(input_tokens), SUM(output_tokens), SUM(reasoning_tokens), SUM(cached_tokens),
SUM(cache_read_tokens), SUM(cache_creation_tokens), SUM(total_tokens),
COALESCE(SUM(known_cost_nano),0), SUM(unpriced_tokens)
FROM events WHERE attempt_id IN (
`+selector+`)
GROUP BY (requested_at_ns / ?), key_id, provider, model, credential_id,
endpoint_class, auth_type, service_tier_used, service_tier_requested, succeeded, error_class,
upstream_status_code, token_quality,
CASE WHEN latency_ms < 100 THEN '<100ms' WHEN latency_ms < 500 THEN '100-499ms' WHEN latency_ms < 1000 THEN '500-999ms' ELSE '1000ms+' END,
CASE WHEN cached_tokens > 0 THEN 'cached' ELSE 'uncached' END, import_batch_id
ON CONFLICT DO UPDATE SET
first_activity_ns=MIN(first_activity_ns,excluded.first_activity_ns),
last_activity_ns=MAX(last_activity_ns,excluded.last_activity_ns),
proxy_requests=proxy_requests+excluded.proxy_requests,
upstream_attempts=upstream_attempts+excluded.upstream_attempts,
input_tokens=input_tokens+excluded.input_tokens, output_tokens=output_tokens+excluded.output_tokens,
reasoning_tokens=reasoning_tokens+excluded.reasoning_tokens, cached_tokens=cached_tokens+excluded.cached_tokens,
cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens,
cache_creation_tokens=cache_creation_tokens+excluded.cache_creation_tokens,
total_tokens=total_tokens+excluded.total_tokens, known_cost_nano=known_cost_nano+excluded.known_cost_nano,
unpriced_tokens=unpriced_tokens+excluded.unpriced_tokens`, rollupArgs...)
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, nil, fmt.Errorf("build hourly retention rollup: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO request_rollups (`+requestRollupColumns+`)
SELECT 'hourly', (requested_at_ns / ?) * ?, ((requested_at_ns / ?) + 1) * ?,
proxy_request_id, key_id, provider, model, COALESCE(credential_id,''), endpoint_class,
COALESCE(auth_type,''), COALESCE(service_tier_used,service_tier_requested,''), succeeded,
COALESCE(error_class,''), COALESCE(upstream_status_code,0), token_quality,
CASE WHEN latency_ms < 100 THEN '<100ms' WHEN latency_ms < 500 THEN '100-499ms' WHEN latency_ms < 1000 THEN '500-999ms' ELSE '1000ms+' END,
CASE WHEN cached_tokens > 0 THEN 'cached' ELSE 'uncached' END, COALESCE(import_batch_id,'')
FROM events WHERE attempt_id IN (
`+selector+`)`, append([]any{hourNanoseconds, hourNanoseconds, hourNanoseconds, hourNanoseconds}, selectorArgs...)...); err != nil {
		_ = tx.Rollback()
		return 0, 0, nil, fmt.Errorf("build hourly request identities: %w", err)
	}
	deleteResult, err := tx.ExecContext(ctx, `DELETE FROM events WHERE attempt_id IN (`+selector+`)`, selectorArgs...)
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, nil, fmt.Errorf("delete retained analytics events: %w", err)
	}
	rolled, _ := rollupResult.RowsAffected()
	deleted, _ := deleteResult.RowsAffected()
	if checkpoint.Valid {
		if err := recordCutoffTx(ctx, tx, "raw_checkpoint", checkpoint.Int64); err != nil {
			_ = tx.Rollback()
			return 0, 0, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, nil, fmt.Errorf("commit raw retention batch: %w", err)
	}
	if !checkpoint.Valid {
		return rolled, deleted, nil, nil
	}
	value := time.Unix(0, checkpoint.Int64).UTC()
	s.advanceEventRetentionFloor(value.Add(time.Nanosecond))
	return rolled, deleted, &value, nil
}

func (s *SQLiteStore) rollHourlyBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, int64, *time.Time, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("begin hourly retention batch: %w", err)
	}
	selector := `SELECT DISTINCT bucket_start_ns FROM rollups WHERE grain='hourly'
AND ((bucket_start_ns / ?) + 1) * ? <= ? ORDER BY bucket_start_ns LIMIT ?`
	selectorArgs := []any{dayNanoseconds, dayNanoseconds, cutoff.UnixNano(), batchSize}
	var checkpoint sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(bucket_start_ns) FROM rollups WHERE grain='hourly' AND bucket_start_ns IN (`+selector+`)`, selectorArgs...).Scan(&checkpoint); err != nil {
		_ = tx.Rollback()
		return 0, 0, nil, fmt.Errorf("read hourly retention checkpoint: %w", err)
	}
	rollupArgs := append([]any{dayNanoseconds, dayNanoseconds, dayNanoseconds, dayNanoseconds}, selectorArgs...)
	rollupArgs = append(rollupArgs, dayNanoseconds)
	rollupResult, err := tx.ExecContext(ctx, `INSERT INTO rollups (`+rollupColumns+`)
SELECT 'daily', (bucket_start_ns / ?) * ?, ((bucket_start_ns / ?) + 1) * ?,
MIN(first_activity_ns), MAX(last_activity_ns), key_id,
provider, model, credential_id, endpoint_class, auth_type, service_tier, succeeded,
error_class, status_code, token_quality, latency_bucket, cache_class,
import_batch_id,
SUM(proxy_requests), SUM(upstream_attempts),
SUM(input_tokens), SUM(output_tokens), SUM(reasoning_tokens), SUM(cached_tokens),
SUM(cache_read_tokens), SUM(cache_creation_tokens), SUM(total_tokens), SUM(known_cost_nano),
SUM(unpriced_tokens) FROM rollups WHERE grain='hourly' AND bucket_start_ns IN (`+selector+`)
GROUP BY (bucket_start_ns / ?), key_id, provider, model, credential_id, endpoint_class,
auth_type, service_tier, succeeded, error_class, status_code, token_quality, latency_bucket, cache_class, import_batch_id
ON CONFLICT DO UPDATE SET proxy_requests=proxy_requests+excluded.proxy_requests,
first_activity_ns=MIN(first_activity_ns,excluded.first_activity_ns),
last_activity_ns=MAX(last_activity_ns,excluded.last_activity_ns),
upstream_attempts=upstream_attempts+excluded.upstream_attempts,
input_tokens=input_tokens+excluded.input_tokens, output_tokens=output_tokens+excluded.output_tokens,
reasoning_tokens=reasoning_tokens+excluded.reasoning_tokens, cached_tokens=cached_tokens+excluded.cached_tokens,
cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens,
cache_creation_tokens=cache_creation_tokens+excluded.cache_creation_tokens,
total_tokens=total_tokens+excluded.total_tokens, known_cost_nano=known_cost_nano+excluded.known_cost_nano,
unpriced_tokens=unpriced_tokens+excluded.unpriced_tokens`, rollupArgs...)
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, nil, fmt.Errorf("build daily retention rollup: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO request_rollups (`+requestRollupColumns+`)
SELECT 'daily', (bucket_start_ns / ?) * ?, ((bucket_start_ns / ?) + 1) * ?,
proxy_request_id, key_id, provider, model, credential_id, endpoint_class, auth_type,
service_tier, succeeded, error_class, status_code, token_quality, latency_bucket, cache_class, import_batch_id FROM request_rollups
WHERE grain='hourly' AND bucket_start_ns IN (`+selector+`)`, append([]any{dayNanoseconds, dayNanoseconds,
		dayNanoseconds, dayNanoseconds}, selectorArgs...)...); err != nil {
		_ = tx.Rollback()
		return 0, 0, nil, fmt.Errorf("build daily request identities: %w", err)
	}
	deleteResult, err := tx.ExecContext(ctx, `DELETE FROM rollups WHERE grain='hourly' AND bucket_start_ns IN (`+selector+`)`, selectorArgs...)
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, nil, fmt.Errorf("delete hourly analytics rollups: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM request_rollups WHERE grain='hourly' AND bucket_start_ns IN (`+selector+`)`, selectorArgs...); err != nil {
		_ = tx.Rollback()
		return 0, 0, nil, fmt.Errorf("delete hourly request rollups: %w", err)
	}
	rolled, _ := rollupResult.RowsAffected()
	deleted, _ := deleteResult.RowsAffected()
	if checkpoint.Valid {
		if err := recordCutoffTx(ctx, tx, "hourly_checkpoint", checkpoint.Int64); err != nil {
			_ = tx.Rollback()
			return 0, 0, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, nil, fmt.Errorf("commit hourly retention batch: %w", err)
	}
	if !checkpoint.Valid {
		return rolled, deleted, nil, nil
	}
	value := time.Unix(0, checkpoint.Int64).UTC()
	return rolled, deleted, &value, nil
}

func recordCutoffTx(ctx context.Context, tx *sql.Tx, grain string, cutoff int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO retention_state(grain, completed_cutoff_ns) VALUES (?, ?)
ON CONFLICT(grain) DO UPDATE SET completed_cutoff_ns=MAX(retention_state.completed_cutoff_ns,excluded.completed_cutoff_ns)`, grain, cutoff)
	if err != nil {
		return fmt.Errorf("record retention checkpoint: %w", err)
	}
	return nil
}

func (s *SQLiteStore) recordCompletedCutoff(ctx context.Context, grain string, cutoff time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO retention_state(grain, completed_cutoff_ns) VALUES (?, ?)
ON CONFLICT(grain) DO UPDATE SET completed_cutoff_ns=MAX(retention_state.completed_cutoff_ns,excluded.completed_cutoff_ns)`, grain, cutoff.UnixNano())
	if err != nil {
		return fmt.Errorf("record completed retention cutoff: %w", err)
	}
	return nil
}

func (s *SQLiteStore) RetentionCutoff() *time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.retentionCutoff.IsZero() {
		return nil
	}
	value := s.retentionCutoff
	return &value
}

func verifyDatabase(ctx context.Context, path string) error {
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return fmt.Errorf("open backup for verification: %w", err)
	}
	defer func() { _ = db.Close() }()
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("verify backup integrity: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("%w: backup: %s", ErrCorruptDatabase, result)
	}
	return nil
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s for checksum: %w", filepath.Base(path), err)
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("checksum %s: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func copyDatabase(ctx context.Context, source, destination string) error {
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(source)}).String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return fmt.Errorf("open restore source: %w", err)
	}
	defer func() { _ = db.Close() }()
	connection, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire restore source connection: %w", err)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.Raw(func(driverConnection any) error {
		backuper, ok := driverConnection.(backupDriver)
		if !ok {
			return fmt.Errorf("SQLite driver does not expose online backup")
		}
		backup, err := backuper.NewBackup(destination)
		if err != nil {
			return err
		}
		for more := true; more; {
			if err := ctx.Err(); err != nil {
				_ = backup.Finish()
				return err
			}
			more, err = backup.Step(128)
			if err != nil {
				_ = backup.Finish()
				return err
			}
		}
		return backup.Finish()
	}); err != nil {
		return fmt.Errorf("stage restored analytics database: %w", err)
	}
	return nil
}

func newRestoreID() (string, error) {
	value := make([]byte, 6)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate restore ID: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func (s *SQLiteStore) databaseSize() int64 {
	usage, err := s.storageUsage()
	if err != nil {
		return fileSize(s.config.Path) + fileSize(s.config.Path+"-wal") + fileSize(s.config.Path+"-shm")
	}
	return usage
}

func modelIdentityFingerprint(identity []byte) (string, error) {
	if len(identity) != 32 {
		return "", fmt.Errorf("identity key must contain 32 bytes")
	}
	digest := sha256.Sum256(identity)
	return hex.EncodeToString(digest[:]), nil
}
