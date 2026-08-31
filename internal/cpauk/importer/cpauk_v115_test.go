package importer

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
	_ "modernc.org/sqlite"
)

func TestCPAUKV115AdapterDedupeSanitizeAndVerifiedImport(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "upstream.db")
	createCPAUKV115Fixture(t, sourcePath)
	source, err := OpenCPAUKV115Source(ctx, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	rows, done, err := source.Read(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !done || len(rows) != 2 {
		t.Fatalf("rows=%d done=%t", len(rows), done)
	}
	first, firstOK := rows[0].Value.(CPAUKV115Row)
	second, secondOK := rows[1].Value.(CPAUKV115Row)
	if !firstOK || !secondOK || first.Origin != "hot" || first.TotalTokens != 10 || second.Origin != "archive" || second.TotalTokens != 20 {
		t.Fatalf("stable dedupe rows = %+v", rows)
	}
	if _, err := source.db.ExecContext(ctx, "DELETE FROM usage_events"); err == nil {
		t.Fatal("read-only source accepted a write")
	}

	destination, err := store.Open(ctx, store.Config{Path: filepath.Join(directory, "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = destination.Close(context.Background()) }()
	backupPath := filepath.Join(directory, "before-import.db")
	worker := Importer{Destination: destination, Transform: NewCPAUKV115Transformer(destination.IdentityKeyArray(), false)}
	result, err := worker.RunWithBackup(ctx, source, Options{BatchID: "batch-upstream", ChunkSize: 1}, backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.RowsRead != 2 || result.Inserted != 2 || !result.Reconciled {
		t.Fatalf("import result = %+v", result)
	}
	if _, err := store.VerifyBackup(ctx, backupPath, backupPath+".manifest.json"); err != nil {
		t.Fatal(err)
	}
	query := model.Query{SchemaVersion: 1, Operation: model.OperationEvents,
		Start: time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		TimeZone: "UTC", PageSize: 10}
	page, err := destination.Events(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Events[0].Tokens.Total != 20 || page.Events[1].Tokens.Total != 10 {
		t.Fatalf("imported events = %+v", page.Events)
	}
	for _, event := range page.Events {
		if event.KeyID != model.KeyID("sk-upstream-secret") || event.KeyID == "sk-upstream-secret" {
			t.Fatalf("raw key escaped sanitizer: %q", event.KeyID)
		}
	}
	databaseBytes, err := os.ReadFile(filepath.Join(directory, "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}
	if containsBytes(databaseBytes, []byte("sk-upstream-secret")) {
		t.Fatal("raw API key was persisted")
	}
}

func TestCommittedImportRequiresBackup(t *testing.T) {
	source := &SliceSource{SourceKind: CPAUKV115SourceKind, ID: "fixture"}
	worker := Importer{Destination: &memoryDestination{events: map[string]model.Event{}, checkpoints: map[string][]byte{}}, Transform: fixtureTransform}
	if _, err := worker.RunWithBackup(context.Background(), source, Options{BatchID: "batch"}, ""); err == nil {
		t.Fatal("committed import without backup was accepted")
	}
}

func createCPAUKV115Fixture(t *testing.T, path string) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	definition := `(id INTEGER PRIMARY KEY,event_key TEXT NOT NULL,api_group_key TEXT NOT NULL,
provider TEXT NOT NULL,endpoint TEXT NOT NULL,auth_type TEXT NOT NULL,request_id TEXT NOT NULL,
model TEXT NOT NULL,model_alias TEXT,service_tier TEXT NOT NULL,response_service_tier TEXT NOT NULL,
executor_type TEXT NOT NULL,timestamp TEXT NOT NULL,auth_index TEXT NOT NULL,failed INTEGER NOT NULL,
generate INTEGER,latency_ms INTEGER NOT NULL,ttft_ms INTEGER,input_tokens INTEGER NOT NULL,
output_tokens INTEGER NOT NULL,reasoning_tokens INTEGER NOT NULL,cached_tokens INTEGER NOT NULL,
cache_read_tokens INTEGER NOT NULL,cache_creation_tokens INTEGER NOT NULL,total_tokens INTEGER NOT NULL)`
	if _, err := database.Exec("CREATE TABLE usage_events " + definition); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("CREATE TABLE usage_events_archive " + definition); err != nil {
		t.Fatal(err)
	}
	insert := func(table string, id int64, eventKey, timestamp string, total int64) {
		t.Helper()
		_, err := database.Exec(`INSERT INTO `+table+` VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, eventKey, "sk-upstream-secret", "openai", "/v1/responses", "oauth", "upstream-request",
			"gpt-fixture", nil, "default", "default", "openai", timestamp, "auth-1", false, true,
			25, 5, total, 0, 0, 0, 0, 0, total)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("usage_events", 1, "duplicate-event", "2026-08-31T04:00:00Z", 10)
	insert("usage_events_archive", 1, "duplicate-event", "2026-08-31T04:00:00Z", 999)
	insert("usage_events_archive", 2, "archive-event", "2026-08-31T05:00:00Z", 20)
}

func containsBytes(data, pattern []byte) bool {
	if len(pattern) == 0 || len(pattern) > len(data) {
		return false
	}
	for index := 0; index+len(pattern) <= len(data); index++ {
		match := true
		for offset := range pattern {
			if data[index+offset] != pattern[offset] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
