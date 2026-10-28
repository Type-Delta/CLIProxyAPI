package legacylog

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/ingest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/outbox"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/store"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestR2GzipRotationDoesNotDuplicateCheckpointedRecord(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	dir := t.TempDir()
	st, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := ingest.New(st, dir, 0, 30)
	at := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	path := makeLog(t, dir, "abcdefab", at)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(filepath.Base(path), raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = ing.Process(ctx, r); err != nil {
		t.Fatal(err)
	}
	detail, _ := json.Marshal(checkpoint{Last: filepath.Base(path), LastAt: at, FilesSeen: 1, Imported: 1, Records: 1})
	if err = db.SetJob(ctx, store.LegacyJob, "running", string(detail)); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path + ".gz")
	if err != nil {
		t.Fatal(err)
	}
	g := gzip.NewWriter(f)
	if _, err = g.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = RunJob(ctx, db, ing, dir); err != nil {
		t.Fatal(err)
	}
	var count uint64
	if err = db.Conn().QueryRow(ctx, "SELECT count() FROM `"+db.Database()+"`.requests").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("gzip rotation replayed checkpointed request with different ID: rows=%d, want 1", count)
	}
}

type r2CutQueryReader struct {
	io.Reader
	cut   *atomic.Bool
	seen  []byte
	close func()
}

func (r *r2CutQueryReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.seen = append(r.seen, p[:n]...)
	if strings.Contains(string(r.seen), "WHERE request_id =") && r.cut.CompareAndSwap(false, true) {
		r.close()
		return 0, io.EOF
	}
	if len(r.seen) > 16384 {
		r.seen = r.seen[len(r.seen)-16384:]
	}
	return n, err
}
func TestR2TransientDisconnectDoesNotSkipLegacyRecord(t *testing.T) {
	ctx := context.Background()
	root := chtest.Handle(t)
	rootDB, err := root.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("CPA_TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		out, e := exec.Command("docker", "port", "cpa-test-clickhouse", "9000/tcp").Output()
		if e != nil {
			t.Fatal(e)
		}
		dsn = "clickhouse://" + strings.TrimSpace(strings.Split(string(out), "\n")[0])
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	target := u.Host
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var cut atomic.Bool
	go func() {
		for {
			client, e := listener.Accept()
			if e != nil {
				return
			}
			go func() {
				defer client.Close()
				up, e := net.Dial("tcp", target)
				if e != nil {
					return
				}
				defer up.Close()
				done := make(chan struct{})
				go func() {
					defer close(done)
					_, _ = io.Copy(up, &r2CutQueryReader{Reader: client, cut: &cut, close: func() { _ = client.Close(); _ = up.Close() }})
					_ = up.Close()
				}()
				_, _ = io.Copy(client, up)
				_ = client.Close()
				<-done
			}()
		}
	}()
	u.Host = listener.Addr().String()
	h := clickhouse.NewHandle(clickhouse.Config{DSN: u.String(), Database: rootDB.Database()})
	defer h.Close()
	db, err := h.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	makeLog(t, dir, "feedface", time.Now())
	ing := ingest.New(st, dir, 0, 30)
	err = RunJob(ctx, db, ing, dir)
	if !cut.Load() {
		t.Fatal("fault injection did not trigger")
	}
	if err != nil {
		return
	} // Retrying is acceptable; falsely declaring completion is not.
	job, _, err := db.Job(ctx, store.LegacyJob)
	if err != nil {
		t.Fatal(err)
	}
	var cp checkpoint
	if err = json.Unmarshal([]byte(job.Detail), &cp); err != nil {
		t.Fatal(err)
	}
	var count uint64
	if err = db.Conn().QueryRow(ctx, "SELECT count() FROM `"+db.Database()+"`.requests").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if job.State == "done" && count == 0 {
		t.Fatalf("one transient disconnect permanently skipped record: state=%s imported=%d skipped=%d error=%q", job.State, cp.Imported, cp.Skipped, cp.Error)
	}
}

func TestR2RecoveredHeaderTimestampDoesNotSkipUnimportedFile(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	dir := t.TempDir()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	makeLog(t, dir, "11112222", at.Add(24*time.Hour))
	path := makeLog(t, dir, "33334444", at)
	if err := os.Chtimes(path, at.Add(48*time.Hour), at.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceTimestamp(path); err == nil {
		t.Skip("process bypasses file permissions")
	}
	if err := db.EnsureJob(ctx, store.LegacyJob); err != nil {
		t.Fatal(err)
	}
	sink := &fakeSink{}
	if err := RunJob(ctx, db, sink, dir); err == nil {
		t.Fatal("expected retryable file-open failure")
	}
	if len(sink.records) != 0 {
		t.Fatalf("must not checkpoint with an unreadable sort key: records=%d", len(sink.records))
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := RunJob(ctx, db, sink, dir); err != nil {
		t.Fatal(err)
	}
	job, _, err := db.Job(ctx, store.LegacyJob)
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.records) != 2 {
		t.Fatalf("restored header moved unimported file behind checkpoint: job=%s records=%d want=2 detail=%s", job.State, len(sink.records), job.Detail)
	}
}

func TestR2TransientDisconnectDoesNotQuarantineOutboxRecord(t *testing.T) {
	ctx := context.Background()
	root := chtest.Handle(t)
	rootDB, err := root.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("CPA_TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		out, e := exec.Command("docker", "port", "cpa-test-clickhouse", "9000/tcp").Output()
		if e != nil {
			t.Fatal(e)
		}
		dsn = "clickhouse://" + strings.TrimSpace(strings.Split(string(out), "\n")[0])
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	target := u.Host
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var cut atomic.Bool
	go func() {
		for {
			client, e := listener.Accept()
			if e != nil {
				return
			}
			go func() {
				defer client.Close()
				up, e := net.Dial("tcp", target)
				if e != nil {
					return
				}
				defer up.Close()
				done := make(chan struct{})
				go func() {
					defer close(done)
					_, _ = io.Copy(up, &r2CutQueryReader{Reader: client, cut: &cut, close: func() { _ = client.Close(); _ = up.Close() }})
					_ = up.Close()
				}()
				_, _ = io.Copy(client, up)
				_ = client.Close()
				<-done
			}()
		}
	}()
	u.Host = listener.Addr().String()
	h := clickhouse.NewHandle(clickhouse.Config{DSN: u.String(), Database: rootDB.Database()})
	defer h.Close()
	db, err := h.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = outbox.Write(dir, &record.Request{RequestID: "transient-disconnect", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ing := ingest.New(st, dir, 0, 30)
	err = ing.Drain(ctx)
	if !cut.Load() {
		t.Fatal("fault injection did not trigger")
	}
	paths, listErr := outbox.List(dir)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(paths) != 1 {
		t.Fatalf("transient dropped lookup lost outbox file: files=%v drain error=%v", paths, err)
	}
	if err = ing.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	paths, _ = outbox.List(dir)
	if len(paths) != 0 {
		t.Fatal("outbox did not recover")
	}
	var n uint64
	if err = db.Conn().QueryRow(ctx, "SELECT count() FROM `"+db.Database()+"`.requests").Scan(&n); err != nil || n != 1 {
		t.Fatalf("recovered count=%d %v", n, err)
	}
}
