package legacylog

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	ch "github.com/ClickHouse/clickhouse-go/v2"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/ingest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/store"
)

type fakeSink struct {
	records []*record.Request
	err     error
	cancel  context.CancelFunc
}

func (s *fakeSink) IngestRecords(_ context.Context, r []*record.Request) error {
	if s.err != nil {
		return s.err
	}
	s.records = append(s.records, r...)
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	return nil
}
func makeLog(t *testing.T, dir, id string, at time.Time) string {
	t.Helper()
	l := logging.NewFileRequestLogger(true, dir, dir, 0)
	if err := l.LogRequest("/v1/messages", "POST", nil, []byte(`{"messages":[{"role":"user","content":"hello"}]}`), 200, nil, []byte("ok"), nil, nil, nil, nil, nil, id, at, at); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*"+id+".log"))
	if err != nil || len(files) != 1 {
		t.Fatalf("files %v %v", files, err)
	}
	return files[0]
}
func TestJobTransitionsAndSourceSafety(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	dir := t.TempDir()
	a := makeLog(t, dir, "aaaaaaaa", time.Now().Add(-time.Hour))
	b := makeLog(t, dir, "bbbbbbbb", time.Now())
	before := map[string][32]byte{}
	mtimes := map[string]time.Time{}
	for _, path := range []string{a, b} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = sha256.Sum256(data)
		info, _ := os.Stat(path)
		mtimes[path] = info.ModTime()
	}
	sink := &fakeSink{}
	if err := RunJob(ctx, db, sink, dir); err != nil || len(sink.records) != 0 {
		t.Fatalf("absent job: %v", err)
	}
	if err := db.EnsureJob(ctx, store.LegacyJob); err != nil {
		t.Fatal(err)
	}
	if err := RunJob(ctx, db, sink, filepath.Join(a, "not-a-directory")); err == nil {
		t.Fatal("expected failure")
	}
	job, _, _ := db.Job(ctx, store.LegacyJob)
	if job.State != "failed" {
		t.Fatalf("state %s", job.State)
	}
	sink.err = nil
	cancelCtx, cancel := context.WithCancel(ctx)
	sink.cancel = cancel
	if err := RunJob(cancelCtx, db, sink, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if err := RunJob(ctx, db, sink, dir); err != nil {
		t.Fatal(err)
	}
	job, _, _ = db.Job(ctx, store.LegacyJob)
	if job.State != "done" || len(sink.records) != 3 {
		t.Fatalf("job %+v records %d", job, len(sink.records))
	}
	if sink.records[0].RequestID != sink.records[1].RequestID || !strings.HasSuffix(sink.records[2].RequestID, "-bbbbbbbb") || sink.records[2].ProxyRequestID != "" {
		t.Fatalf("replay/order %+v", sink.records)
	}
	if err := RunJob(ctx, db, sink, dir); err != nil || len(sink.records) != 3 {
		t.Fatalf("done retry %v", err)
	}
	for _, path := range []string{a, b} {
		data, _ := os.ReadFile(path)
		info, _ := os.Stat(path)
		if sha256.Sum256(data) != before[path] || !info.ModTime().Equal(mtimes[path]) {
			t.Fatalf("source changed: %s", path)
		}
	}
}
func TestRealIngester(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	makeLog(t, dir, "cccccccc", time.Now())
	ing := ingest.New(s, dir, 0, 30)
	if err = RunJob(ctx, db, ing, dir); err != nil {
		t.Fatal(err)
	}
	var n uint64
	err = db.Conn().QueryRow(ctx, "SELECT count() FROM `"+db.Database()+"`.requests WHERE source = 'legacy_log'").Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("requests %d: %v", n, err)
	}
}
func TestGzipAndCorruptCorpus(t *testing.T) {
	dir := t.TempDir()
	path := makeLog(t, dir, "dddddddd", time.Now())
	b, _ := os.ReadFile(path)
	gzpath := path + ".gz"
	f, err := os.Create(gzpath)
	if err != nil {
		t.Fatal(err)
	}
	g := gzip.NewWriter(f)
	_, _ = g.Write(b)
	_ = g.Close()
	_ = f.Close()
	got, err := readSource(gzpath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Parse(filepath.Base(gzpath), got); err != nil {
		t.Fatal(err)
	}
	for _, b := range [][]byte{nil, []byte("garbage"), []byte("=== REQUEST INFO ===\nURL: /\n"), []byte("=== REQUEST INFO ===\nTimestamp: bad\n\n=== HEADERS ===\n")} {
		if _, err := Parse("bad.log", b); err == nil {
			t.Fatalf("accepted %q", b)
		}
	}
	if err := os.WriteFile(gzpath, []byte("bad gzip"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSource(gzpath); !isCorrupt(err) {
		t.Fatalf("gzip: %v", err)
	}
}

type pausedSink struct{ calls int }

func (s *pausedSink) IngestRecords(_ context.Context, _ []*record.Request) error {
	s.calls++
	if s.calls == 1 {
		return fmt.Errorf("%w: low disk", record.ErrIngestPaused)
	}
	return nil
}
func TestPausedJobRetries(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	dir := t.TempDir()
	makeLog(t, dir, "eeeeeeee", time.Now())
	if err := db.EnsureJob(ctx, store.LegacyJob); err != nil {
		t.Fatal(err)
	}
	sink := &pausedSink{}
	if err := RunJob(ctx, db, sink, dir); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 2 {
		t.Fatalf("calls %d", sink.calls)
	}
	job, _, err := db.Job(ctx, store.LegacyJob)
	if err != nil || job.State != "done" {
		t.Fatalf("job %+v: %v", job, err)
	}
}

func TestLegacyRotatedCheckpointResumesBySortKey(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	dir := t.TempDir()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	a := makeLog(t, dir, "11111111", at)
	makeLog(t, dir, "22222222", at.Add(time.Second))
	cp := fmt.Sprintf(`{"last":%q,"last_at":"2026-10-01T00:00:00Z","files_seen":1,"imported":1,"records":1}`, filepath.Base(a))
	if err := db.SetJob(ctx, store.LegacyJob, "running", cp); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	sink := &fakeSink{}
	if err := RunJob(ctx, db, sink, dir); err != nil {
		t.Fatal(err)
	}
	if len(sink.records) != 1 || !strings.HasSuffix(sink.records[0].RequestID, "22222222") {
		t.Fatal(sink.records)
	}
}
func TestLegacyPersistentRecordFailureIsSkipped(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	dir := t.TempDir()
	makeLog(t, dir, "33333333", time.Now())
	if err := db.EnsureJob(ctx, store.LegacyJob); err != nil {
		t.Fatal(err)
	}
	if err := RunJob(ctx, db, &fakeSink{err: fmt.Errorf("%w: invalid stored record", record.ErrInvalidRecord)}, dir); err != nil {
		t.Fatal(err)
	}
	job, _, err := db.Job(ctx, store.LegacyJob)
	var cp checkpoint
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(job.Detail), &cp); err != nil {
		t.Fatal(err)
	}
	if job.State != "done" || cp.Skipped != 1 || cp.Imported != 0 || cp.Attempts != 3 {
		t.Fatalf("%+v %+v", job, cp)
	}
}
func TestLegacyLargeBodyAllocationsAreLinear(t *testing.T) {
	body := "=== REQUEST INFO ===\nTimestamp: 2026-10-01T00:00:00Z\nURL: /v1/messages\nMethod: POST\n\n=== HEADERS ===\n\n=== REQUEST BODY ===\n" + strings.Repeat("readable line\n", 20000) + "\n=== RESPONSE ===\nStatus: 200\n\nok\n"
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := Parse("large.log", []byte(body)); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > uint64(len(body))*30 {
		t.Fatalf("allocated %d for %d-byte log", allocated, len(body))
	}
}

type retryableOnceSink struct {
	err   error
	calls int
}

func (s *retryableOnceSink) IngestRecords(context.Context, []*record.Request) error {
	s.calls++
	if s.calls == 1 {
		return s.err
	}
	return nil
}
func TestLegacyUnknownAndCapacityErrorsRetry(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	for _, errFirst := range []error{io.EOF, errors.New("unrecognized driver failure"), &ch.Exception{Code: 252}, &ch.Exception{Code: 241}, &ch.Exception{Code: 164}, &ch.Exception{Code: 159}} {
		dir := t.TempDir()
		makeLog(t, dir, "retry123", time.Now())
		if err := db.SetJob(ctx, store.LegacyJob, "pending", ""); err != nil {
			t.Fatal(err)
		}
		sink := &retryableOnceSink{err: errFirst}
		if err := RunJob(ctx, db, sink, dir); err != nil {
			t.Fatal(err)
		}
		job, _, err := db.Job(ctx, store.LegacyJob)
		if err != nil {
			t.Fatal(err)
		}
		var cp checkpoint
		if err = json.Unmarshal([]byte(job.Detail), &cp); err != nil {
			t.Fatal(err)
		}
		if sink.calls != 2 || cp.Skipped != 0 || cp.Imported != 1 || cp.Attempts != 2 {
			t.Fatalf("retryable %v lost record: calls=%d checkpoint=%+v", errFirst, sink.calls, cp)
		}
	}
}
