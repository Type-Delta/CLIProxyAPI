package chstore

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

type archiveTable struct {
	Schema string            `json:"schema"`
	Rows   []json.RawMessage `json:"rows"`
}
type analyticsArchive struct {
	Format string                  `json:"format"`
	Tables map[string]archiveTable `json:"tables"`
}

func (s *Store) tableSchema(ctx context.Context, table string) (string, error) {
	rows, err := s.db.Conn().Query(ctx, "SELECT name,type FROM system.columns WHERE database=? AND table=? ORDER BY position", s.db.Database(), table)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var columns []string
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return "", err
		}
		columns = append(columns, "`"+strings.ReplaceAll(name, "`", "``")+"` "+typ)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(columns) == 0 {
		return "", fmt.Errorf("missing table %s", table)
	}
	return strings.Join(columns, ", "), nil
}

func (s *Store) Backup(ctx context.Context, destination string) (BackupManifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return BackupManifest{}, ErrClosed
	}
	return s.backup(ctx, destination)
}

func (s *Store) backup(ctx context.Context, destination string) (BackupManifest, error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return BackupManifest{}, err
	}
	f, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("create backup file: %w", err)
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	hash := sha256.New()
	out := bufio.NewWriter(io.MultiWriter(f, hash))
	if _, err := out.WriteString(`{"format":"cpa-clickhouse-analytics-v1","tables":{`); err != nil {
		return BackupManifest{}, err
	}
	for index, table := range analyticsTables {
		schema, err := s.tableSchema(ctx, table)
		if err != nil {
			return BackupManifest{}, fmt.Errorf("describe %s: %w", table, err)
		}
		nameJSON, _ := json.Marshal(table)
		schemaJSON, _ := json.Marshal(schema)
		if index != 0 {
			if err := out.WriteByte(','); err != nil {
				return BackupManifest{}, err
			}
		}
		if _, err := fmt.Fprintf(out, "%s:{\"schema\":%s,\"rows\":[", nameJSON, schemaJSON); err != nil {
			return BackupManifest{}, err
		}
		selection := s.table(table)
		if filter := s.c4bTablePublicationFilter(table); filter != "" {
			selection += " FINAL WHERE is_deleted=0" + filter
		}
		rows, err := s.db.Conn().Query(ctx, "SELECT formatRow('JSONEachRow', *) FROM "+selection)
		if err != nil {
			return BackupManifest{}, fmt.Errorf("export %s: %w", table, err)
		}
		first := true
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				_ = rows.Close()
				return BackupManifest{}, err
			}
			if !first {
				if err := out.WriteByte(','); err != nil {
					_ = rows.Close()
					return BackupManifest{}, err
				}
			}
			first = false
			if _, err := out.WriteString(strings.TrimSpace(raw)); err != nil {
				_ = rows.Close()
				return BackupManifest{}, err
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return BackupManifest{}, err
		}
		if err := rows.Close(); err != nil {
			return BackupManifest{}, err
		}
		if _, err := out.WriteString("]}"); err != nil {
			return BackupManifest{}, err
		}
	}
	if _, err := out.WriteString("}}"); err != nil {
		return BackupManifest{}, err
	}
	if err := out.Flush(); err != nil {
		return BackupManifest{}, err
	}
	if err := f.Sync(); err != nil {
		return BackupManifest{}, err
	}
	if err := f.Close(); err != nil {
		return BackupManifest{}, err
	}
	fingerprint, err := model.IdentityKeyFingerprint(s.identityKey)
	if err != nil {
		return BackupManifest{}, err
	}
	keyDigest := sha256.Sum256(s.identityKey)
	manifest := BackupManifest{SchemaVersion: s.currentSchema, IdentityEpoch: s.identityEpoch, DatabaseFile: filepath.Base(destination), DatabaseSHA256: hex.EncodeToString(hash.Sum(nil)), IdentityKeyFile: filepath.Base(destination) + ".identity.key", IdentityKeySHA256: hex.EncodeToString(keyDigest[:]), IdentityKeyFingerprint: fingerprint, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return BackupManifest{}, err
	}
	files := []struct {
		path string
		data []byte
	}{{destination + ".identity.key", s.identityKey}, {destination + ".manifest.json", append(manifestData, '\n')}}
	var created []string
	for _, item := range files {
		f, err := os.OpenFile(item.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			for _, path := range created {
				_ = os.Remove(path)
			}
			return BackupManifest{}, fmt.Errorf("create backup file: %w", err)
		}
		created = append(created, item.path)
		_, err = f.Write(item.data)
		if err == nil {
			err = f.Sync()
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			for _, path := range created {
				_ = os.Remove(path)
			}
			return BackupManifest{}, fmt.Errorf("write backup file: %w", err)
		}
	}
	ok = true
	return manifest, nil
}

func (s *Store) VerifyBackup(ctx context.Context, databasePath, manifestPath string) (BackupManifest, error) {
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return BackupManifest{}, err
	}
	var manifest BackupManifest
	decoder := json.NewDecoder(bytes.NewReader(manifestData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return BackupManifest{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return BackupManifest{}, fmt.Errorf("backup manifest contains trailing data")
	}
	if manifest.SchemaVersion < 1 || manifest.DatabaseFile != filepath.Base(databasePath) || manifest.IdentityKeyFile != filepath.Base(databasePath)+".identity.key" || !model.IsCorrelationID(manifest.IdentityEpoch) {
		return BackupManifest{}, fmt.Errorf("invalid backup manifest inventory")
	}
	if _, err := time.Parse(time.RFC3339Nano, manifest.CreatedAt); err != nil {
		return BackupManifest{}, fmt.Errorf("invalid backup timestamp: %w", err)
	}
	if err := readArchive(ctx, databasePath, manifest.DatabaseSHA256, nil); err != nil {
		return BackupManifest{}, err
	}
	key, err := os.ReadFile(filepath.Join(filepath.Dir(manifestPath), manifest.IdentityKeyFile))
	if err != nil {
		return BackupManifest{}, err
	}
	fingerprint, err := model.IdentityKeyFingerprint(key)
	if err != nil || fingerprint != manifest.IdentityKeyFingerprint || fingerprint != manifest.IdentityKeySHA256 {
		return BackupManifest{}, fmt.Errorf("backup identity mismatch")
	}
	return manifest, ctx.Err()
}

func (s *Store) Restore(ctx context.Context, databasePath, manifestPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	if err := s.requireLegacyImportDone(ctx); err != nil {
		return err
	}
	return s.restore(ctx, databasePath, manifestPath)
}

func (s *Store) restore(ctx context.Context, databasePath, manifestPath string) error {
	manifest, err := s.VerifyBackup(ctx, databasePath, manifestPath)
	if err != nil {
		return err
	}
	if manifest.SchemaVersion != s.currentSchema {
		return ErrUnsupportedSchema
	}
	key, err := os.ReadFile(filepath.Join(filepath.Dir(manifestPath), manifest.IdentityKeyFile))
	if err != nil {
		return err
	}
	keyFingerprint, err := model.IdentityKeyFingerprint(key)
	if err != nil || keyFingerprint != manifest.IdentityKeyFingerprint {
		return fmt.Errorf("backup key changed during verification")
	}
	id, err := model.NewCorrelationID()
	if err != nil {
		return err
	}
	stages := make(map[string]string)
	defer func() {
		for _, name := range stages {
			_ = s.db.Conn().Exec(context.Background(), "DROP TABLE IF EXISTS "+s.table(name)+" SYNC")
		}
	}()
	var chunk strings.Builder
	var stage, schema string
	flush := func() error {
		if chunk.Len() == 0 {
			return nil
		}
		if hook, ok := ctx.Value(archiveChunkTestHookKey{}).(func(int)); ok {
			hook(chunk.Len())
		}
		queryCtx := ch.Context(ctx, ch.WithSettings(ch.Settings{"max_query_size": uint64(2*chunk.Len() + 2*len(schema) + 4096)}))
		err := s.db.Conn().Exec(queryCtx, "INSERT INTO "+s.table(stage)+" SELECT * FROM format(JSONEachRow, ?, ?)", schema, chunk.String())
		chunk.Reset()
		return err
	}
	err = readArchive(ctx, databasePath, manifest.DatabaseSHA256, func(table, archiveSchema string, row json.RawMessage) error {
		if row == nil {
			if err := flush(); err != nil {
				return err
			}
			var err error
			schema, err = s.tableSchema(ctx, table)
			if err != nil {
				return err
			}
			if schema != archiveSchema {
				return fmt.Errorf("archive schema does not match %s", table)
			}
			stage = "c4_restore_" + id + "_" + table
			if err := s.db.Conn().Exec(ctx, "CREATE TABLE "+s.table(stage)+" AS "+s.table(table)); err != nil {
				return err
			}
			stages[table] = stage
			return nil
		}
		if chunk.Len() > 0 && chunk.Len()+len(row)+1 > archiveChunkBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		chunk.Write(row)
		chunk.WriteByte('\n')
		if chunk.Len() >= archiveChunkBytes {
			return flush()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("stage analytics archive: %w", err)
	}
	if err := flush(); err != nil {
		return err
	}
	var fingerprint, epoch string
	if err := s.db.Conn().QueryRow(ctx, "SELECT value FROM "+s.table(stages["analytics_metadata"])+" FINAL WHERE key='identity_fingerprint' AND is_deleted=0").Scan(&fingerprint); err != nil {
		return err
	}
	if err := s.db.Conn().QueryRow(ctx, "SELECT value FROM "+s.table(stages["analytics_metadata"])+" FINAL WHERE key='identity_epoch' AND is_deleted=0").Scan(&epoch); err != nil {
		return err
	}
	if fingerprint != manifest.IdentityKeyFingerprint || epoch != manifest.IdentityEpoch {
		return fmt.Errorf("backup metadata identity mismatch")
	}
	var cutoff time.Time
	metadataRows, err := s.db.Conn().Query(ctx, "SELECT key,value FROM "+s.table(stages["analytics_metadata"])+" FINAL WHERE is_deleted=0 AND key IN ('retention_raw_cutoff_ns','retention_raw_checkpoint_ns')")
	if err != nil {
		return err
	}
	for metadataRows.Next() {
		var name, value string
		if err := metadataRows.Scan(&name, &value); err != nil {
			_ = metadataRows.Close()
			return err
		}
		ns, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			_ = metadataRows.Close()
			return fmt.Errorf("invalid retention boundary: %w", err)
		}
		candidate := time.Unix(0, ns).UTC()
		if name == "retention_raw_checkpoint_ns" {
			candidate = candidate.Add(time.Nanosecond)
		}
		if candidate.After(cutoff) {
			cutoff = candidate
		}
	}
	if err := metadataRows.Err(); err != nil {
		_ = metadataRows.Close()
		return err
	}
	if err := metadataRows.Close(); err != nil {
		return err
	}
	book := s.config.PriceBook
	var generation uint64
	err = s.db.Conn().QueryRow(ctx, "SELECT generation FROM "+s.table(stages["analytics_control_heads"])+" FINAL WHERE namespace='pricing'").Scan(&generation)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		var raw string
		err = s.db.Conn().QueryRow(ctx, "SELECT value FROM "+s.table(stages["analytics_control"])+" FINAL WHERE namespace='pricing' AND generation=? AND key='snapshot' AND is_deleted=0", generation).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			var snapshot PricingSnapshot
			if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
				return fmt.Errorf("invalid pricing snapshot: %w", err)
			}
			book = aggregate.PriceBook{Rules: snapshot.Rules, KiroUSDPerCredit: snapshot.KiroUSDPerCredit}
		}
	}
	var maxVersion uint64
	for _, table := range analyticsTables {
		columns, err := s.db.Conn().Query(ctx, "SELECT name FROM system.columns WHERE database=? AND table=? AND name IN ('version','sequence') AND type='UInt64'", s.db.Database(), stages[table])
		if err != nil {
			return fmt.Errorf("inspect restore version columns for %s: %w", table, err)
		}
		var names []string
		for columns.Next() {
			var name string
			if err := columns.Scan(&name); err != nil {
				_ = columns.Close()
				return err
			}
			names = append(names, name)
		}
		if err := columns.Err(); err != nil {
			_ = columns.Close()
			return err
		}
		if err := columns.Close(); err != nil {
			return err
		}
		for _, name := range names {
			var version uint64
			if err := s.db.Conn().QueryRow(ctx, "SELECT max(`"+name+"`) FROM "+s.table(stages[table])).Scan(&version); err != nil {
				return fmt.Errorf("read restored %s %s: %w", table, name, err)
			}
			maxVersion = max(maxVersion, version)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Stage the key and journal the UUIDs before publishing any table.
	publishCtx := context.WithoutCancel(ctx)
	job, found, err := s.db.Job(publishCtx, restoreJob)
	if err != nil {
		return err
	}
	if found && job.State == "running" {
		return fmt.Errorf("analytics restore recovery is required")
	}
	keyPath := filepath.Join(filepath.Dir(s.config.IdentityKeyPath), ".c4-restore-key-"+id)
	keyFile, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = keyFile.Write(key); err == nil {
		err = keyFile.Sync()
	}
	if closeErr := keyFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(keyPath)
		return err
	}
	dir, err := os.Open(filepath.Dir(keyPath))
	if err != nil {
		_ = os.Remove(keyPath)
		return err
	}
	err = dir.Sync()
	if closeErr := dir.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(keyPath)
		return err
	}
	journal := restoreJournal{KeyFile: keyPath, Fingerprint: fingerprint}
	for _, table := range analyticsTables {
		oldUUID, err := s.tableUUID(publishCtx, table)
		if err != nil {
			_ = os.Remove(keyPath)
			return err
		}
		newUUID, err := s.tableUUID(publishCtx, stages[table])
		if err != nil {
			_ = os.Remove(keyPath)
			return err
		}
		journal.Tables = append(journal.Tables, restoreTable{Name: table, Stage: stages[table], OldUUID: oldUUID, NewUUID: newUUID})
	}
	detail, err := json.Marshal(journal)
	if err != nil {
		_ = os.Remove(keyPath)
		return err
	}
	if err := s.db.SetJob(publishCtx, restoreJob, "running", string(detail)); err != nil {
		_ = os.Remove(keyPath)
		return err
	}
	// The journal owns the staged tables after publication starts.
	stages = nil
	hook, _ := ctx.Value(restoreTestHookKey{}).(func(int) error)
	if err := s.finishRestore(publishCtx, &journal, hook); err != nil {
		s.db = nil
		return fmt.Errorf("restore interrupted; reopen to recover: %w", err)
	}
	s.identityKey, s.identityEpoch, s.retentionCutoff, s.config.PriceBook = key, epoch, cutoff, book
	for old := s.version.Load(); maxVersion > old; old = s.version.Load() {
		if s.version.CompareAndSwap(old, maxVersion) {
			break
		}
	}
	return nil
}
