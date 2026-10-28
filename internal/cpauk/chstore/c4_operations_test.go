package chstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/importer"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/maintenance"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

func TestC4MaintenanceAndCheckpoints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	insertFixtureEvents(t, s)
	if err := s.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LoadImportCheckpoint(ctx, "missing"); err != nil || found {
		t.Fatalf("missing checkpoint: found=%v err=%v", found, err)
	}
	for _, id := range []string{"one", "two"} {
		if err := s.SaveImportCheckpoint(ctx, id, "source", "fingerprint", 12, 2, []byte(id), true, [5]int64{1, 2, 3, 4, 5}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveImportCheckpoint(ctx, "one", "source", "fingerprint", 13, 3, []byte("new"), false, [5]int64{}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"one": "new", "two": "two"} {
		got, found, err := s.LoadImportCheckpoint(ctx, id)
		if err != nil || !found || !bytes.Equal(got, []byte(want)) {
			t.Fatalf("checkpoint %s: %q found=%v err=%v", id, got, found, err)
		}
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LoadImportCheckpoint(ctx, "one"); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed: %v", err)
	}
}

func TestC4ImporterCheckpointResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	events := loadFixtureEvents(t)
	rows := make([]importer.SourceRow, len(events))
	for i := range events {
		rows[i].Value = events[i]
	}
	source := func() *importer.SliceSource {
		return &importer.SliceSource{SourceKind: "fixture", ID: "c4", Rows: rows}
	}
	worker := importer.Importer{Destination: s, Transform: func(_ context.Context, row importer.SourceRow) (model.Event, bool, error) {
		return row.Value.(model.Event), false, nil
	}}
	result, err := worker.Run(ctx, source(), importer.Options{BatchID: "c4-test", ChunkSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Inserted != int64(len(events)) || !result.Reconciled {
		t.Fatalf("first import: %+v", result)
	}
	result, err = worker.Run(ctx, source(), importer.Options{BatchID: "c4-test", ChunkSize: 2, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Inserted != int64(len(events)) || !result.Reconciled {
		t.Fatalf("resumed import: %+v", result)
	}
	actual, err := s.loadEvents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(actual) != len(events) {
		t.Fatalf("got %d events, want %d", len(actual), len(events))
	}
}

func TestC4PurgeAndRollbackRemoveOutcomeReferences(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	events := loadFixtureEvents(t)
	events[1].ProxyRequestID = events[0].ProxyRequestID
	if events[0].KeyID == events[1].KeyID {
		events[1].KeyID = model.KeyID("another key")
	}
	if err := s.WriteBatchWithPatches(ctx, events[:2], []model.ProxyResponsePatch{{ProxyRequestID: events[0].ProxyRequestID, StatusCode: 429}}); err != nil {
		t.Fatal(err)
	}
	count, err := s.PreviewPurgeByKeyID(ctx, events[0].KeyID)
	if err != nil || count != 1 {
		t.Fatalf("preview=%d err=%v", count, err)
	}
	removed, err := s.PurgeByKeyID(ctx, events[0].KeyID)
	if err != nil || removed != count {
		t.Fatalf("purge=%d err=%v", removed, err)
	}
	if err := s.WriteBatch(ctx, events[:1]); err != nil {
		t.Fatal(err)
	}
	got, err := s.loadEvents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range got {
		if event.AttemptID == events[0].AttemptID && event.ProxyStatusCode != nil {
			t.Fatalf("purged outcome inherited: %+v", event)
		}
		if event.AttemptID == events[1].AttemptID && (event.ProxyStatusCode == nil || *event.ProxyStatusCode != 429) {
			t.Fatalf("surviving outcome lost: %+v", event)
		}
	}
	if _, err := s.WriteImportBatch(ctx, events[2:3], "batch-c4"); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteBatchWithPatches(ctx, nil, []model.ProxyResponsePatch{{ProxyRequestID: events[2].ProxyRequestID, StatusCode: 503}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveImportCheckpoint(ctx, "batch-c4", "fixture", "fingerprint", 1, 1, []byte("state"), true, [5]int64{}); err != nil {
		t.Fatal(err)
	}
	removed, err = s.RollbackImport(ctx, "batch-c4")
	if err != nil || removed != 1 {
		t.Fatalf("rollback=%d err=%v", removed, err)
	}
	if _, found, err := s.LoadImportCheckpoint(ctx, "batch-c4"); found || err != nil {
		t.Fatalf("checkpoint found=%v err=%v", found, err)
	}
	if _, err := s.WriteImportBatch(ctx, events[2:3], "batch-c4"); err != nil {
		t.Fatal(err)
	}
	got, err = s.loadEvents(ctx, "attempt_id=?", events[2].AttemptID)
	if err != nil || len(got) != 1 || got[0].ProxyStatusCode != nil {
		t.Fatalf("reimport=%+v err=%v", got, err)
	}
}

func TestC4BackupRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	if err := s.db.Conn().Exec(ctx, "CREATE TABLE "+s.table("requests")+" (id String) ENGINE=MergeTree ORDER BY id"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Conn().Exec(ctx, "INSERT INTO "+s.table("requests")+" VALUES ('telemetry')"); err != nil {
		t.Fatal(err)
	}
	events := insertFixtureEvents(t, s)
	if err := s.db.Conn().Exec(ctx, "INSERT INTO "+s.table("analytics_quota_history")+" (provider,credential_id,window_id,observed_at_ns,remaining_percent,plan_id,source,row_id) VALUES ('provider','credential','window',1,37.5,'plan','cpamc','quota-1')"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "analytics.backup")
	manifest, err := s.Backup(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyBackup(ctx, path, path+".manifest.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeByKeyID(ctx, events[0].KeyID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Conn().Exec(ctx, "TRUNCATE TABLE "+s.table("analytics_quota_history")); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore(ctx, path, path+".manifest.json"); err != nil {
		t.Fatal(err)
	}
	got, err := s.loadEvents(ctx, "")
	if err != nil || len(got) != len(events) {
		t.Fatalf("restore count=%d err=%v", len(got), err)
	}
	if s.IdentityEpoch() != manifest.IdentityEpoch {
		t.Fatal("identity epoch changed")
	}
	var percentage float64
	if err := s.db.Conn().QueryRow(ctx, "SELECT remaining_percent FROM "+s.table("analytics_quota_history")+" WHERE row_id='quota-1'").Scan(&percentage); err != nil || percentage != 37.5 {
		t.Fatalf("quota history=%v err=%v", percentage, err)
	}
	var count uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table("requests")).Scan(&count); err != nil || count != 1 {
		t.Fatalf("telemetry rows=%d err=%v", count, err)
	}
}

func TestC4IdentityEpoch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	insertFixtureEvents(t, s)
	if err := s.db.Conn().Exec(ctx, "INSERT INTO "+s.table("analytics_quota_history")+" (provider,row_id) VALUES ('provider','quota-1')"); err != nil {
		t.Fatal(err)
	}
	oldEpoch, oldKey := s.IdentityEpoch(), s.IdentityKeyArray()
	result, err := s.StartNewIdentityEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.IdentityEpoch == oldEpoch || s.IdentityKeyArray() == oldKey {
		t.Fatal("identity did not change")
	}
	got, err := s.loadEvents(ctx, "")
	if err != nil || len(got) != 0 {
		t.Fatalf("new epoch has %d events: %v", len(got), err)
	}
	var quotaCount uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table("analytics_quota_history")).Scan(&quotaCount); err != nil || quotaCount != 0 {
		t.Fatalf("new epoch quota=%d err=%v", quotaCount, err)
	}
	if err := s.Restore(ctx, result.ArchivedDB, result.ArchivedDB+".manifest.json"); err != nil {
		t.Fatal(err)
	}
	got, err = s.loadEvents(ctx, "")
	if err != nil || len(got) == 0 || s.IdentityEpoch() != oldEpoch || s.IdentityKeyArray() != oldKey {
		t.Fatalf("restore epoch: events=%d err=%v", len(got), err)
	}
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table("analytics_quota_history")+" WHERE row_id='quota-1'").Scan(&quotaCount); err != nil || quotaCount != 1 {
		t.Fatalf("restored quota=%d err=%v", quotaCount, err)
	}
}

func TestC4CPAUKV115Importer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "source.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE usage_events (id INTEGER PRIMARY KEY,event_key TEXT NOT NULL,api_group_key TEXT NOT NULL,provider TEXT NOT NULL,endpoint TEXT NOT NULL,auth_type TEXT NOT NULL,request_id TEXT NOT NULL,model TEXT NOT NULL,model_alias TEXT,service_tier TEXT NOT NULL,response_service_tier TEXT NOT NULL,executor_type TEXT NOT NULL,timestamp TEXT NOT NULL,auth_index TEXT NOT NULL,failed INTEGER NOT NULL,generate INTEGER,latency_ms INTEGER NOT NULL,ttft_ms INTEGER,input_tokens INTEGER NOT NULL,output_tokens INTEGER NOT NULL,reasoning_tokens INTEGER NOT NULL,cached_tokens INTEGER NOT NULL,cache_read_tokens INTEGER NOT NULL,cache_creation_tokens INTEGER NOT NULL,total_tokens INTEGER NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO usage_events VALUES (1,'event','secret','openai','/v1/responses','oauth','request','gpt-test',NULL,'default','default','openai','2026-08-31T04:00:00Z','auth',0,1,25,5,10,0,0,0,0,0,10)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := importer.OpenCPAUKV115Source(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	worker := importer.Importer{Destination: s, Transform: importer.NewCPAUKV115Transformer(s.IdentityKeyArray(), false)}
	result, err := worker.RunWithBackup(ctx, source, importer.Options{BatchID: "upstream", ChunkSize: 1}, filepath.Join(dir, "backup"))
	if err != nil || result.Inserted != 1 || !result.Reconciled {
		t.Fatalf("import=%+v err=%v", result, err)
	}
	events, err := s.loadEvents(ctx, "")
	if err != nil || len(events) != 1 || events[0].KeyID != model.KeyID("secret") || events[0].Tokens.Total != 10 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

type c4MaintenanceAdapter struct {
	maintenance.Store
	db *Store
}

func (a c4MaintenanceAdapter) Backup(ctx context.Context, path string) (store.BackupManifest, error) {
	return a.db.Backup(ctx, path)
}
func (a c4MaintenanceAdapter) Restore(ctx context.Context, path, manifest string) error {
	return a.db.Restore(ctx, path, manifest)
}
func (a c4MaintenanceAdapter) VerifyBackup(ctx context.Context, path, manifest string) (store.BackupManifest, error) {
	return a.db.VerifyBackup(ctx, path, manifest)
}

func TestC4MaintenanceControllerRestoresClickHouseBackup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	events := insertFixtureEvents(t, s)
	ops := maintenance.StoreOperations(c4MaintenanceAdapter{db: s})
	progress := func(int, string) {}
	path := filepath.Join(t.TempDir(), "backup")
	result, err := ops["backup"](ctx, map[string]any{"path": path}, progress)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeByKeyID(ctx, events[0].KeyID); err != nil {
		t.Fatal(err)
	}
	_, err = ops["restore"](ctx, map[string]any{"path": path, "manifest": path + ".manifest.json", "backup_id": result["backup_id"]}, progress)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.loadEvents(ctx, "")
	if err != nil || len(got) != len(events) {
		t.Fatalf("restored=%d err=%v", len(got), err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(" "); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = ops["restore"](ctx, map[string]any{"path": path, "manifest": path + ".manifest.json", "backup_id": result["backup_id"]}, progress)
	if !errors.Is(err, maintenance.ErrBackupInvalid) {
		t.Fatalf("invalid backup error: %v", err)
	}
}

func TestC4RemoveRetainedHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	for _, table := range []string{"analytics_rollups", "analytics_request_rollups"} {
		for _, entry := range []struct{ key, batch string }{{"key-a", "batch-a"}, {"key-b", "batch-b"}} {
			query := "INSERT INTO " + s.table(table) + " (grain,bucket_start_ns,key_id,import_batch_id,version,is_deleted) VALUES (?,?,?,?,?,0)"
			if err := s.db.Conn().Exec(ctx, query, "hourly", int64(1), entry.key, entry.batch, s.nextVersion()); err != nil {
				t.Fatal(err)
			}
		}
	}
	count, err := s.PreviewPurgeByKeyID(ctx, "key-a")
	if err != nil || count != 2 {
		t.Fatalf("preview=%d err=%v", count, err)
	}
	count, err = s.PurgeByKeyID(ctx, "key-a")
	if err != nil || count != 2 {
		t.Fatalf("purge=%d err=%v", count, err)
	}
	count, err = s.RollbackImport(ctx, "batch-b")
	if err != nil || count != 2 {
		t.Fatalf("rollback=%d err=%v", count, err)
	}
}

func TestC4RestoreRejectsModifiedBackup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	events := insertFixtureEvents(t, s)
	path := filepath.Join(t.TempDir(), "backup")
	if _, err := s.Backup(ctx, path); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(" "); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore(ctx, path, path+".manifest.json"); err == nil {
		t.Fatal("accepted modified backup")
	}
	got, err := s.loadEvents(ctx, "")
	if err != nil || len(got) != len(events) {
		t.Fatalf("live events=%d err=%v", len(got), err)
	}
}

func TestC4PurgeClearsOrphanedOutcomes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	event := loadFixtureEvents(t)[0]
	if err := s.WriteBatchWithPatches(ctx, []model.Event{event}, []model.ProxyResponsePatch{{ProxyRequestID: event.ProxyRequestID, StatusCode: 429}}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Conn().Exec(ctx, "ALTER TABLE "+s.table("analytics_events")+" DELETE WHERE attempt_id=? SETTINGS mutations_sync=2", event.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeByKeyID(ctx, event.KeyID); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	got, err := s.loadEvents(ctx, "attempt_id=?", event.AttemptID)
	if err != nil || len(got) != 1 || got[0].ProxyStatusCode != nil {
		t.Fatalf("reinserted=%+v err=%v", got, err)
	}
}
