package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/outbox"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/store"
)

func TestS4BatchReplayAndLookup(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	now := time.Now().UTC()
	var records []*record.Request
	for n := 0; n < 3; n++ {
		records = append(records, &record.Request{RequestID: fmt.Sprintf("batch-%d", n), ProxyRequestID: fmt.Sprintf("proxy-%d", n), StartedAt: now.Add(time.Duration(n) * time.Second), URL: "/v1/messages", Detail: "error", Body: []byte(fmt.Sprintf(`{"messages":[{"role":"user","content":"hello %d"}]}`, n)), Attempts: []record.Attempt{{Index: 1, URL: "/v1/messages", Body: []byte(`{"messages":[{"role":"user","content":"attempt"}]}`)}}})
	}
	if err = ing.IngestRecords(ctx, records); err != nil {
		t.Fatal(err)
	}
	if err = db.Conn().Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatal(err)
	}
	var inserts, probes uint64
	dbName := "`" + db.Database() + "`."
	err = db.Conn().QueryRow(ctx, "SELECT countIf(startsWith(query, 'INSERT INTO') AND position(query, 'schema_migrations') = 0 AND position(query, 'cpa_jobs') = 0),countIf(startsWith(query, 'SELECT ') AND position(query, ' IN (') > 0 AND (position(query, 'transcript_content') > 0 OR position(query, 'transcript_entries') > 0)) FROM system.query_log WHERE type='QueryFinish' AND position(query, ?) > 0", dbName).Scan(&inserts, &probes)
	if err != nil || inserts != 5 || probes != 2 {
		t.Fatalf("inserts=%d probes=%d err=%v", inserts, probes, err)
	}
	var markerLast bool
	err = db.Conn().QueryRow(ctx, "SELECT maxIf(event_time_microseconds, position(query, '.requests (') > 0) >= maxIf(event_time_microseconds, position(query, '.transcript_content (') > 0 OR position(query, '.transcript_entries (') > 0 OR position(query, '.request_contexts (') > 0 OR position(query, '.attempts (') > 0) FROM system.query_log WHERE type='QueryFinish' AND startsWith(query,'INSERT INTO') AND position(query, ?) > 0", dbName).Scan(&markerLast)
	if err != nil || !markerLast {
		t.Fatalf("request completion marker was not last: %v", err)
	}
	for _, id := range []string{"batch-1", "atch-1", "proxy-1"} {
		r, e := store.Request(ctx, db, id)
		if e != nil || r.RequestID != "batch-1" {
			t.Fatalf("lookup %s: %v %v", id, r, e)
		}
	}
	logs, err := store.ErrorLogs(ctx, db)
	if err != nil || len(logs) != 3 {
		t.Fatalf("logs %v: %v", logs, err)
	}
	query := "WITH error_requests AS (SELECT * FROM " + dbName + "requests FINAL WHERE detail='error') SELECT r.request_id,length(r.response_body)+length(r.request_body_raw)+length(r.request_headers)+length(r.response_headers)+length(r.websocket_timeline)+ifNull(c.bytes,0)+ifNull(a.bytes,0) FROM error_requests r LEFT JOIN (SELECT x.request_id,sum(c.bytes) AS bytes FROM (SELECT request_id,arrayJoin(content_ids) AS content_id FROM " + dbName + "request_contexts FINAL WHERE request_id IN (SELECT request_id FROM error_requests)) x INNER JOIN (SELECT content_id,bytes FROM " + dbName + "transcript_content FINAL) c ON x.content_id=c.content_id GROUP BY x.request_id) c ON r.request_id=c.request_id LEFT JOIN (SELECT request_id,sum(length(response_body)+length(request_body_raw)+length(request_headers)+length(response_headers)+length(websocket_timeline)) AS bytes FROM " + dbName + "attempts FINAL WHERE request_id IN (SELECT request_id FROM error_requests) GROUP BY request_id) a ON r.request_id=a.request_id ORDER BY r.started_at DESC"
	rows, err := db.Conn().Query(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	for _, log := range logs {
		var id string
		var size uint64
		if !rows.Next() {
			t.Fatal("missing old log")
		}
		if err = rows.Scan(&id, &size); err != nil {
			t.Fatal(err)
		}
		if log.Name != "error-"+id+".log" || log.Size != int64(size) {
			t.Fatalf("parity: %v %s %d", log, id, size)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	// Simulate a failure before the completion marker was written.
	if err = db.Conn().Exec(ctx, "ALTER TABLE "+dbName+"requests DELETE WHERE request_id='batch-0' SETTINGS mutations_sync=2"); err != nil {
		t.Fatal(err)
	}
	if err = New(s, t.TempDir(), 0, 30).IngestRecords(ctx, records[:1]); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"request_contexts", "attempts"} {
		var n uint64
		if err = db.Conn().QueryRow(ctx, "SELECT count() FROM "+dbName+table+" WHERE request_id='batch-0'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		want := uint64(1)
		if table == "request_contexts" {
			want = 2
		}
		if n != want {
			t.Fatalf("%s replay count %d", table, n)
		}
	}
}

func TestS4BatchFailurePreservesFiles(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ing := New(s, dir, 0, 30)
	base := time.Now().UTC()
	for n := 1; n <= 2; n++ {
		if err = outbox.Write(dir, &record.Request{RequestID: fmt.Sprintf("good-%d", n), StartedAt: base.Add(time.Duration(n) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	invalid := encoder.EncodeAll([]byte(`{"request_id":""}`), nil)
	encoder.Close()
	if err = os.WriteFile(filepath.Join(dir, "telemetry-outbox", fmt.Sprintf("%d-invalid.json.zst", base.Add(1500*time.Millisecond).UnixNano())), invalid, 0600); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if err = ing.Drain(ctx); !errors.Is(err, ErrPaused) {
			t.Fatalf("attempt %d: %v", n+1, err)
		}
		files, e := outbox.List(dir)
		if e != nil || len(files) != 2 {
			t.Fatalf("files %v %v", files, e)
		}
	}
	if err = ing.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	files, err := outbox.List(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("remaining %v %v", files, err)
	}
	failed, err := os.ReadDir(filepath.Join(dir, "telemetry-outbox", "failed"))
	if err != nil || len(failed) != 1 {
		t.Fatalf("failed %v %v", failed, err)
	}
	var count uint64
	if err = db.Conn().QueryRow(ctx, "SELECT count() FROM `"+db.Database()+"`.requests WHERE startsWith(request_id,'good-')").Scan(&count); err != nil || count != 2 {
		t.Fatalf("count %d %v", count, err)
	}
}
