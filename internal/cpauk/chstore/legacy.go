package chstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

const c4bLegacyJob = "legacy-import:analytics"

type c4bLegacyProgress struct {
	Fingerprint  string           `json:"fingerprint"`
	After        string           `json:"after_attempt_id"`
	Tables       map[string]bool  `json:"completed_tables"`
	Counts       map[string]int64 `json:"counts"`
	SessionSince int64            `json:"session_capture_since_ns"`
}

// RunLegacyImport imports CPA's analytics database once, yielding intake and reads
// between durable chunks. Maintenance must wait for this job before mutation.
// A read-only source connection makes a consistent SQLite snapshot; only that
// private temporary copy is migrated. Each durable chunk precedes its checkpoint.
// Replacement keys make a replay visible exactly once without background merges.
func RunLegacyImport(ctx context.Context, s *Store, sqlitePath string) (err error) {
	if s == nil {
		return ErrClosed
	}
	s.mu.Lock()
	locked, jobStarted := true, false
	defer func() {
		if !locked {
			s.mu.Lock()
		}
		s.clearHistory()
		s.historyImporting = false
		if err != nil && jobStarted && s.db != nil {
			err = errors.Join(err, s.db.SetJob(context.WithoutCancel(ctx), c4bLegacyJob, "failed", err.Error()))
		}
		s.mu.Unlock()
	}()
	if s.db == nil {
		return ErrClosed
	}
	job, found, err := s.db.Job(ctx, c4bLegacyJob)
	if err != nil {
		return err
	}
	if !found || job.State != "pending" && job.State != "running" && job.State != "failed" {
		return nil
	}
	if err = s.db.SetJob(ctx, c4bLegacyJob, "running", ""); err != nil {
		return err
	}
	jobStarted = true
	s.historyImporting = true
	progress := c4bLegacyProgress{Tables: map[string]bool{}, Counts: map[string]int64{}, SessionSince: time.Now().UTC().UnixNano()}
	controls, err := s.loadControl(ctx, "retention")
	if err != nil {
		return err
	}
	if raw, ok := controls[c4bLegacyJob]; ok {
		if err = json.Unmarshal(raw, &progress); err != nil {
			return err
		}
	}
	save := func() error {
		raw, err := json.Marshal(progress)
		if err != nil {
			return err
		}
		controls[c4bLegacyJob] = raw
		if err := s.replaceControl(ctx, "retention", controls); err != nil {
			return err
		}
		detail, err := json.Marshal(progress.Counts)
		if err != nil {
			return err
		}
		if err = s.db.SetJob(ctx, c4bLegacyJob, "running", string(detail)); err != nil {
			return err
		}
		return s.yieldLegacyImport(ctx)
	}
	if _, err = os.Stat(sqlitePath); errors.Is(err, os.ErrNotExist) {
		zeroCounts := map[string]int64{}
		for _, table := range []string{"events", "rollups", "request_rollups", "quota_history", "pricing_rules", "pricing_manual_rules", "pricing_catalog_rules", "pricing_provenance", "pricing_catalog_provenance", "pricing_catalog_bindings", "pricing_catalog_providers", "key_lifecycle", "provider_quota_snapshots", "analytics_metadata", "retention_state", "import_checkpoints"} {
			zeroCounts[table] = 0
		}
		detail, _ := json.Marshal(zeroCounts)
		return s.db.SetJob(ctx, c4bLegacyJob, "done", string(detail))
	} else if err != nil {
		return err
	}
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(sqlitePath)}).String() + "?mode=ro"
	source, err := sql.Open("sqlite", uri)
	if err != nil {
		return fmt.Errorf("legacy analytics database is unreadable: %w", err)
	}
	defer func() { _ = source.Close() }()
	// Reading and migrating the private SQLite source does not hold up CH intake.
	s.mu.Unlock()
	locked = false
	var version int
	if err = source.QueryRowContext(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations").Scan(&version); err != nil {
		return fmt.Errorf("legacy analytics database is unreadable: %w", err)
	}
	if version >= 18 {
		if err = source.QueryRowContext(ctx, "SELECT applied_at_ns FROM schema_migrations WHERE version=18").Scan(&progress.SessionSince); err != nil {
			return fmt.Errorf("legacy analytics database is unreadable: %w", err)
		}
	}
	fingerprint, err := c4bSourceFingerprint(sqlitePath)
	if err != nil {
		return err
	}
	if progress.Fingerprint != "" && progress.Fingerprint != fingerprint {
		return fmt.Errorf("legacy source changed since import checkpoint")
	}
	progress.Fingerprint = fingerprint
	s.mu.Lock()
	locked = true
	if s.db == nil {
		return ErrClosed
	}
	if err = save(); err != nil {
		return err
	}
	config := s.config
	identityKey := append([]byte(nil), s.identityKey...)
	identityEpoch := s.identityEpoch
	s.mu.Unlock()
	locked = false
	// Keeping temporary files under the user's tmp directory also avoids filling
	// the small system /tmp with a potentially large legacy analytics snapshot.
	temporaryRoot := filepath.Join(os.Getenv("HOME"), "tmp")
	if err = os.MkdirAll(temporaryRoot, 0700); err != nil {
		return err
	}
	directory, err := os.MkdirTemp(temporaryRoot, "cpauk-legacy-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	copyPath := filepath.Join(directory, "analytics.db")
	if _, err = source.ExecContext(ctx, "VACUUM INTO ?", copyPath); err != nil {
		return fmt.Errorf("snapshot read-only legacy analytics: %w", err)
	}
	// Detect a changing source rather than attaching a checkpoint to a different
	// snapshot. The legacy source must remain stable during the import.
	afterFingerprint, err := c4bSourceFingerprint(sqlitePath)
	if err != nil {
		return err
	}
	if afterFingerprint != fingerprint {
		return fmt.Errorf("legacy source changed while taking snapshot")
	}
	keyPath := filepath.Join(directory, "identity.key")
	if err = os.WriteFile(keyPath, identityKey, 0600); err != nil {
		return err
	}
	config.Path = copyPath
	config.IdentityKeyPath = keyPath
	config.MaxStorageBytes = 0
	config.MinFreeBytes = 1
	config.PricingFetcher = nil
	config.PriceBook = aggregate.PriceBook{}
	legacy, err := store.Open(ctx, config)
	if err != nil {
		return fmt.Errorf("migrate private legacy copy: %w", err)
	}
	defer func() { _ = legacy.Close(context.Background()) }()
	if legacy.IdentityEpoch() != identityEpoch {
		return ErrIdentityKeyMismatch
	}
	copyDB, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: copyPath}).String()+"?mode=ro")
	if err != nil {
		return err
	}
	defer func() { _ = copyDB.Close() }()
	// A SQLite batch may have stopped partway through an hour or day. Complete
	// those partially retained buckets on the disposable copy before publication.
	var rawEnd, dailyEnd int64
	if err = copyDB.QueryRowContext(ctx, "SELECT coalesce(max(bucket_end_ns),0) FROM rollups").Scan(&rawEnd); err != nil {
		return err
	}
	if err = copyDB.QueryRowContext(ctx, "SELECT coalesce(max(bucket_end_ns),0) FROM rollups WHERE grain='daily'").Scan(&dailyEnd); err != nil {
		return err
	}
	if floor := legacy.RetentionCutoff(); floor != nil && floor.UnixNano() > rawEnd {
		rawEnd = floor.UnixNano()
	}
	if rawEnd != 0 {
		start, end, boundsErr := aggregate.BucketBounds(time.Unix(0, rawEnd).UTC(), config.RetentionTimeZone, "1h")
		if boundsErr != nil {
			return boundsErr
		}
		if start.UnixNano() != rawEnd {
			rawEnd = end.UnixNano()
		}
	}
	if rawEnd != 0 {
		var daily time.Time
		if dailyEnd != 0 {
			daily = time.Unix(0, dailyEnd).UTC()
		}
		if _, err = legacy.ApplyRetentionPolicy(ctx, time.Unix(0, rawEnd).UTC(), daily, 10000); err != nil {
			return fmt.Errorf("complete partial legacy buckets: %w", err)
		}
	}
	s.mu.Lock()
	locked = true
	if s.db == nil {
		return ErrClosed
	}
	if !progress.Tables["events"] {
		for {
			rows, errQuery := copyDB.QueryContext(ctx, "SELECT "+c4bEventSelect+" FROM events WHERE attempt_id>? ORDER BY attempt_id LIMIT 1000", progress.After)
			if errQuery != nil {
				return errQuery
			}
			events := []model.Event{}
			for rows.Next() {
				event, errScan := c4bScanEvent(rows)
				if errScan != nil {
					_ = rows.Close()
					return errScan
				}
				events = append(events, event)
			}
			errRows := rows.Err()
			_ = rows.Close()
			if errRows != nil {
				return errRows
			}
			if len(events) == 0 {
				break
			}
			// Preserve recorded prices and ownership while deduplicating and promoting late rows.
			if _, err = s.insertEventsMode(ctx, events, "", false, true); err != nil {
				return err
			}
			if err = c4bStep(ctx, "legacy:events-written"); err != nil {
				return err
			}
			progress.After = events[len(events)-1].AttemptID
			progress.Counts["events"] += int64(len(events))
			if err = save(); err != nil {
				return err
			}
			if err = c4bStep(ctx, "legacy:events-checkpoint"); err != nil {
				return err
			}
		}
		progress.Tables["events"] = true
		if err = save(); err != nil {
			return err
		}
	}
	for _, spec := range []struct{ table, columns string }{{"rollups", c4bRollupColumns}, {"request_rollups", c4bRequestColumns}} {
		if progress.Tables[spec.table] {
			continue
		}
		count, errCopy := s.c4bCopyFacts(ctx, copyDB, spec.table, spec.columns)
		if errCopy != nil {
			return errCopy
		}
		if err = c4bStep(ctx, "legacy:"+spec.table+"-written"); err != nil {
			return err
		}
		progress.Counts[spec.table] = count
		progress.Tables[spec.table] = true
		if err = save(); err != nil {
			return err
		}
	}
	if !progress.Tables["control"] {
		if err = s.c4bImportControl(ctx, legacy, copyDB, progress.Counts); err != nil {
			return err
		}
		if err = c4bStep(ctx, "legacy:control-written"); err != nil {
			return err
		}
		progress.Tables["control"] = true
		if err = save(); err != nil {
			return err
		}
	}
	// Cutoffs are published only after both retained tables and raw events exist.
	// A partial table import is never selected as retained history before this.
	if err = s.c4bImportMetadata(ctx, copyDB, progress.SessionSince); err != nil {
		return err
	}
	if err = c4bStep(ctx, "legacy:published"); err != nil {
		return err
	}
	detail, err := json.Marshal(progress.Counts)
	if err != nil {
		return err
	}
	return s.db.SetJob(ctx, c4bLegacyJob, "done", string(detail))
}

func c4bSourceFingerprint(path string) (string, error) {
	hash := sha256.New()
	for _, suffix := range []string{"", "-wal"} {
		file, err := os.Open(path + suffix)
		if errors.Is(err, os.ErrNotExist) && suffix != "" {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = io.Copy(hash, file)
		closeErr := file.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func (s *Store) c4bCopyFacts(ctx context.Context, db *sql.DB, table, columns string) (int64, error) {
	defer s.clearHistory()
	rows, err := db.QueryContext(ctx, "SELECT "+columns+" FROM "+table)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var count int64
	names := strings.Split(columns, ",")
	for {
		batch, err := s.db.Conn().PrepareBatch(synchronousWriteContext(ctx), "INSERT INTO "+s.table("analytics_"+table)+" ("+columns+",version,is_deleted)")
		if err != nil {
			return count, err
		}
		size := 0
		for size < 1000 && rows.Next() {
			values := make([]any, len(names))
			pointers := make([]any, len(names))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err = rows.Scan(pointers...); err != nil {
				_ = batch.Abort()
				return count, err
			}
			values = append(values, s.nextVersion(), uint8(0))
			if err = batch.Append(values...); err != nil {
				_ = batch.Abort()
				return count, err
			}
			size++
		}
		if err = rows.Err(); err != nil {
			_ = batch.Abort()
			return count, err
		}
		if size == 0 {
			_ = batch.Abort()
			return count, nil
		}
		if err = batch.Send(); err != nil {
			return count, err
		}
		count += int64(size)
		if err := s.yieldLegacyImport(ctx); err != nil {
			return count, err
		}
	}
}

// yieldLegacyImport lets readers and native intake run between durable chunks.
// The caller holds mu; it remains held when this method returns or panics.
func (s *Store) yieldLegacyImport(ctx context.Context) (err error) {
	s.clearHistory()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.db == nil {
			err = ErrClosed
		}
	}()
	runtime.Gosched()
	return c4bStep(ctx, "legacy:yield")
}

func (s *Store) c4bImportMetadata(ctx context.Context, db *sql.DB, since int64) error {
	values := map[string]string{"session_capture_since_ns": strconv.FormatInt(since, 10)}
	rows, err := db.QueryContext(ctx, "SELECT key,value FROM analytics_metadata")
	if err != nil {
		return err
	}
	for rows.Next() {
		var key, value string
		if err = rows.Scan(&key, &value); err != nil {
			_ = rows.Close()
			return err
		}
		if key != "session_capture_since_ns" {
			values[key] = value
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	rows, err = db.QueryContext(ctx, "SELECT grain,completed_cutoff_ns FROM retention_state")
	if err != nil {
		return err
	}
	for rows.Next() {
		var grain string
		var ns int64
		if err = rows.Scan(&grain, &ns); err != nil {
			_ = rows.Close()
			return err
		}
		switch grain {
		case "raw", "hourly":
			values["retention_"+grain+"_cutoff_ns"] = strconv.FormatInt(ns, 10)
		case "raw_checkpoint", "hourly_checkpoint":
			values["retention_"+grain+"_ns"] = strconv.FormatInt(ns, 10)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if err = s.setMetadataBatch(ctx, values); err != nil {
		return err
	}
	for _, key := range []string{"retention_raw_cutoff_ns", "retention_raw_checkpoint_ns"} {
		if text, ok := values[key]; ok {
			ns, errParse := strconv.ParseInt(text, 10, 64)
			if errParse != nil {
				return errParse
			}
			if strings.Contains(key, "checkpoint") {
				ns++
			}
			floor := time.Unix(0, ns).UTC()
			if floor.After(s.retentionCutoff) {
				s.retentionCutoff = floor
			}
		}
	}
	return nil
}

// requireLegacyImportDone guards destructive work while a resumable source remains.
// Callers hold the mutation lock, including the check and subsequent maintenance.
func (s *Store) requireLegacyImportDone(ctx context.Context) error {
	job, found, err := s.db.Job(ctx, c4bLegacyJob)
	if err != nil {
		return err
	}
	if found && job.State != "done" && job.State != "abandoned" {
		return fmt.Errorf("analytics legacy import is %s; destructive maintenance requires done", job.State)
	}
	return nil
}

// AbandonLegacyImport deliberately stops retries without claiming a successful import.
// Running imports must exit before maintenance can abandon their job.
func (s *Store) AbandonLegacyImport(ctx context.Context, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("legacy import abandonment requires a reason")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	job, found, err := s.db.Job(ctx, c4bLegacyJob)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("legacy analytics import job does not exist")
	}
	switch job.State {
	case "running":
		return fmt.Errorf("cannot abandon a running legacy analytics import")
	case "done":
		return fmt.Errorf("legacy analytics import already completed")
	case "abandoned":
		return nil
	}
	detail, err := json.Marshal(map[string]any{"reason": reason, "abandoned_at": time.Now().UTC()})
	if err != nil {
		return err
	}
	return s.db.SetJob(ctx, c4bLegacyJob, "abandoned", string(detail))
}
