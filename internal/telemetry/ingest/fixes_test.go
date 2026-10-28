package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/outbox"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/store"
)

func TestIdleDrainDoesNotAccessDatabase(t *testing.T) {
	// A nil store makes any accidental ping/disk query fail immediately.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("idle drain accessed database: %v", r)
		}
	}()
	ing := New(nil, t.TempDir(), 1, 30)
	ing.nextIdleCheck = time.Now().Add(time.Minute)
	if err := ing.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestDrainQuarantinesBadRecordsAndContinues(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	folder := filepath.Join(dir, "telemetry-outbox")
	if err = os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(folder, "0-corrupt.json.zst"), []byte("truncated"), 0600); err != nil {
		t.Fatal(err)
	}
	// A complete, decodable file whose record cannot be persisted.
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	invalid := encoder.EncodeAll([]byte(`{"request_id":""}`), nil)
	encoder.Close()
	if err = os.WriteFile(filepath.Join(folder, "1-invalid.json.zst"), invalid, 0600); err != nil {
		t.Fatal(err)
	}

	if err = outbox.Write(dir, &record.Request{RequestID: "good", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ing := New(s, dir, 0, 30)
	for attempt := 0; attempt < 3; attempt++ {
		err = ing.Drain(ctx)
		if attempt < 2 && !errors.Is(err, ErrPaused) {
			t.Fatalf("deterministic record should retry before quarantine: %v", err)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	failed, err := os.ReadDir(filepath.Join(folder, "failed"))
	if err != nil || len(failed) != 2 {
		t.Fatalf("quarantine: %v %v", failed, err)
	}
	var n uint64
	if err = db.Conn().QueryRow(ctx, "SELECT count() FROM `"+db.Database()+"`.requests WHERE request_id='good'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("healthy record stalled: %d %v", n, err)
	}
}
func TestSkillOccurrencesClientViewOnTranslatedAttempt(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	r := &record.Request{RequestID: "translated", StartedAt: time.Now(), URL: "/v1/messages", Body: []byte(`{"messages":[{"role":"user","content":"skill AAA content"}]}`), Attempts: []record.Attempt{{Index: 1, URL: "/v1/responses", Body: []byte(`{"input":[{"role":"user","content":"skill AAA content"}]}`)}}}
	if err = New(s, t.TempDir(), 0, 30).Process(ctx, r); err != nil {
		t.Fatal(err)
	}
	// Execute the shipped yardstick example, with database and skill parameters bound.
	data, err := os.ReadFile("../../api/handlers/management/telemetry_query_examples.json")
	if err != nil {
		t.Fatal(err)
	}
	var examples []struct {
		Name string `json:"name"`
		SQL  string `json:"sql"`
	}
	if err = json.Unmarshal(data, &examples); err != nil {
		t.Fatal(err)
	}
	query := ""
	for _, e := range examples {
		if e.Name == "Skill occurrences this month" {
			query = e.SQL
		}
	}
	query = strings.ReplaceAll(query, "transcript_entries", "`"+db.Database()+"`.transcript_entries")
	query = strings.ReplaceAll(query, "transcript_content", "`"+db.Database()+"`.transcript_content")
	query = strings.ReplaceAll(query, "{skill:String}", "'skill AAA content'")
	var n uint64
	if err = db.Conn().QueryRow(ctx, query).Scan(&n); err != nil || n != 1 {
		t.Fatalf("skill counted client and translated views: %d %v", n, err)
	}
}

func TestRunCollectsGarbageAtStartup(t *testing.T) {
	db := chtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Conn().Exec(ctx, "INSERT INTO `"+db.Database()+"`.transcript_content (content_id,kind,raw,text,bytes,first_observed_at) VALUES ('stale','element','{}','',2,now()-INTERVAL 60 DAY)"); err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	done := make(chan struct{})
	go func() { defer close(done); ing.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for {
		var n uint64
		err = db.Conn().QueryRow(ctx, "SELECT count() FROM `"+db.Database()+"`.transcript_content WHERE content_id='stale'").Scan(&n)
		if err != nil {
			t.Fatalf("startup never collected stale content: %v", err)
		}
		if n == 0 {
			break
		}
		runtime.Gosched()
	}
}

func TestIdleDrainDoesNotRefreshLastSuccess(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ing.status.LastSuccess = at
	if err = ing.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if got := ing.Status().LastSuccess; !got.Equal(at) {
		t.Fatalf("idle drain changed last successful ingestion: %s", got)
	}
}

func TestIdleCallerCancellationDoesNotPoisonFinalDrain(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err = ing.Drain(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost caller cancellation: %v", err)
	}
	if status := ing.Status(); status.PausedReason != "" || status.LastError != "" || !ing.nextIdleCheck.IsZero() {
		t.Fatalf("caller cancellation poisoned cached database health: %+v next check=%s", status, ing.nextIdleCheck)
	}
	if err = ing.Drain(ctx); err != nil {
		t.Fatalf("healthy final empty drain failed: %v", err)
	}
	if status := ing.Status(); status.PausedReason != "" || !status.LastSuccess.IsZero() {
		t.Fatalf("final drain health inaccurate: %+v", status)
	}
}
