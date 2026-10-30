package chstore

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestS3MigrationExistingDataAndRetry(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	original, err := migrationFiles.ReadFile("migrations/0001_analytics.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Migrate(ctx, "analytics", fstest.MapFS{"0001_analytics.sql": &fstest.MapFile{Data: original}}); err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db, config: Config{PriceBook: fixturePriceBook()}}
	events := loadFixtureEvents(t)
	if err = s.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	files, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		result, err := db.Migrate(ctx, "analytics", files)
		if err != nil || result.ToVersion != 2 {
			t.Fatalf("migration %+v %v", result, err)
		}
	}
	got, err := s.loadEvents(ctx, "")
	if err != nil || len(got) != len(events) {
		t.Fatalf("migrated events %d %v", len(got), err)
	}
	// Native driver inserts and nullable LowCardinality scans must still work.
	next := events[0]
	next.AttemptID = strings.Repeat("f", 32)
	if err = s.WriteBatch(ctx, []model.Event{next}); err != nil {
		t.Fatal(err)
	}
	var provider string
	var auth *string
	if err = db.Conn().QueryRow(ctx, "SELECT provider,auth_type FROM "+s.table("analytics_events")+" FINAL WHERE attempt_id=?", next.AttemptID).Scan(&provider, &auth); err != nil {
		t.Fatal(err)
	}
	if provider != next.Provider || (auth == nil) != (next.AuthType == nil) {
		t.Fatalf("dimension scan %q %v", provider, auth)
	}
}

func TestS3RepriceCountReusedWithinRun(t *testing.T) {
	ctx := ch.Context(context.Background(), ch.WithSettings(ch.Settings{"log_comment": "s3_reprice_count", "log_queries": 1}))
	s := openTestStore(t)
	events := insertFixtureEvents(t, s)
	start, end := events[0].RequestedAt, events[0].RequestedAt
	for _, e := range events {
		if e.RequestedAt.Before(start) {
			start = e.RequestedAt
		}
		if e.RequestedAt.After(end) {
			end = e.RequestedAt
		}
	}
	if len(events) < 3 {
		t.Fatalf("fixture needs at least three events, got %d", len(events))
	}
	options := RepriceOptions{Range: model.Range{Start: start, End: end.Add(time.Second), TimeZone: "UTC"}, ChunkSize: 1}
	var updated int64
	for {
		result, err := s.Reprice(ctx, options, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.Matched != int64(len(events)) {
			t.Fatalf("matched %+v", result)
		}
		updated += result.Updated
		if result.Completed {
			break
		}
		options.ResumeCheckpoint = result.Checkpoint
		options.Matched = result.Matched
	}
	if updated != int64(len(events)) {
		t.Fatalf("updated %d", updated)
	}
	if err := s.db.Conn().Exec(context.Background(), "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatal(err)
	}
	var counts uint64
	if err := s.db.Conn().QueryRow(context.Background(), "SELECT count() FROM system.query_log WHERE type='QueryFinish' AND log_comment='s3_reprice_count' AND startsWith(query, ?)", "SELECT count() FROM "+s.table("analytics_events")+" FINAL WHERE is_deleted=0 AND requested_at_ns>=").Scan(&counts); err != nil {
		t.Fatal(err)
	}
	if counts != 2 {
		t.Fatalf("full range count queries=%d want first and final only", counts)
	}
}

func TestS3LateRecoveryAfterReopen(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	e := loadFixtureEvents(t)[0]
	if _, err := s.ApplyRetention(ctx, e.RequestedAt.Add(48*time.Hour), 1000); err != nil {
		t.Fatal(err)
	}
	stopped := errors.New("staged source")
	failure := context.WithValue(ctx, c4bStepKey{}, func(step string) error {
		if step == "late:source" {
			return stopped
		}
		return nil
	})
	if err := s.WriteBatch(failure, []model.Event{e}); !errors.Is(err, stopped) {
		t.Fatalf("stage %v", err)
	}
	// A fresh Store has no in-memory knowledge of the interrupted generation.
	recovered := &Store{db: s.db, config: s.config, retentionCutoff: s.retentionCutoff}
	recovered.version.Store(s.version.Load())
	if err := recovered.WriteBatch(ctx, []model.Event{e}); err != nil {
		t.Fatal(err)
	}
	controls, err := recovered.loadControl(ctx, "late_retention")
	if err != nil || len(controls) != 0 {
		t.Fatalf("pending plans %v %v", controls, err)
	}
	if got := c4bVisible(t, recovered); got.Attempts != 1 {
		t.Fatalf("recovered %+v", got)
	}
}

func TestS3EmptyLatePlanPrunesLiveRows(t *testing.T) {
	s := openTestStore(t)
	insertFixtureEvents(t, s)
	if err := s.db.Conn().Exec(context.Background(), "INSERT INTO "+s.table("analytics_events")+" (attempt_id,version,is_deleted,retained_generation) VALUES (?,1,1,1)", strings.Repeat("f", 32)); err != nil {
		t.Fatal(err)
	}
	ctx := ch.Context(context.Background(), ch.WithSettings(ch.Settings{"log_comment": "s3_empty_late", "log_queries": 1}))
	if err := s.c4bRecoverLate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Conn().Exec(context.Background(), "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatal(err)
	}
	var count uint64
	if err := s.db.Conn().QueryRow(context.Background(), "SELECT sum(read_rows) FROM system.query_log WHERE type='QueryFinish' AND log_comment='s3_empty_late' AND has(tables,?)", s.db.Database()+".analytics_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("ordinary recovery read %d rows", count)
	}
}

func TestS3ReindexDoesNotForceFinal(t *testing.T) {
	s := openTestStore(t)
	events := insertFixtureEvents(t, s)
	ctx := ch.Context(context.Background(), ch.WithSettings(ch.Settings{"log_comment": "s3_reindex_" + s.db.Database(), "log_queries": 1}))
	if err := s.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Conn().Exec(context.Background(), "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatal(err)
	}
	var count, forced uint64
	if err := s.db.Conn().QueryRow(context.Background(), "SELECT count(),countIf(position(query,' FINAL')>0) FROM system.query_log WHERE type='QueryFinish' AND log_comment=? AND startsWith(query,'OPTIMIZE TABLE')", "s3_reindex_"+s.db.Database()).Scan(&count, &forced); err != nil {
		t.Fatal(err)
	}
	if count != uint64(len(analyticsTables)) || forced != 0 {
		t.Fatalf("optimize queries=%d forced=%d", count, forced)
	}
	got, err := s.loadEvents(context.Background(), "")
	if err != nil || len(got) != len(events) {
		t.Fatalf("reindex count=%d %v", len(got), err)
	}
}

func TestS3PatchEligibilityAcrossBatches(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	events := loadFixtureEvents(t)
	first := events[0]
	first.ProxyStatusCode = nil
	first.RespondedAt = nil
	retry := first
	retry.AttemptID = strings.Repeat("e", 32)
	recorded := first
	recorded.AttemptID = strings.Repeat("f", 32)
	status := 202
	recorded.ProxyStatusCode = &status
	patch := model.ProxyResponsePatch{ProxyRequestID: first.ProxyRequestID, StatusCode: 429, RespondedAt: first.RequestedAt.Add(time.Second)}
	if err := s.WriteBatchWithPatches(ctx, []model.Event{first, recorded}, []model.ProxyResponsePatch{patch}); err != nil {
		t.Fatal(err)
	}
	patch.StatusCode = 200
	if err := s.WriteBatchWithPatches(ctx, []model.Event{retry}, []model.ProxyResponsePatch{patch}); err != nil {
		t.Fatal(err)
	}
	got, err := s.loadEvents(ctx, "proxy_request_id=?", first.ProxyRequestID)
	if err != nil || len(got) != 3 {
		t.Fatalf("events %d %v", len(got), err)
	}
	want := map[string]int{first.AttemptID: 429, retry.AttemptID: 200, recorded.AttemptID: 202}
	for _, event := range got {
		if event.ProxyStatusCode == nil || *event.ProxyStatusCode != want[event.AttemptID] {
			t.Fatalf("attempt %s status %v", event.AttemptID, event.ProxyStatusCode)
		}
	}
}
