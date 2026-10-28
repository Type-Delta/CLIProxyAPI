package chstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

func c4bLegacyFixture(t *testing.T, retained bool) (Config, []model.Event) {
	t.Helper()
	ctx := context.Background()
	config := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: fixturePriceBook()}
	legacy, err := store.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	events := loadFixtureEvents(t)[:3]
	for i := range events {
		events[i].RequestedAt = time.Date(2026, 8, 10+i, 4, 5, 0, 0, time.UTC)
	}
	events[1].ProxyRequestID = events[0].ProxyRequestID
	if err = legacy.WriteBatchWithPatches(ctx, events, []model.ProxyResponsePatch{{ProxyRequestID: events[2].ProxyRequestID, StatusCode: 503, Error: "recorded proxy failure", RespondedAt: events[2].RequestedAt.Add(time.Second)}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err = legacy.UpdateKeyLifecycle(ctx, []string{events[0].KeyID}, []string{events[1].KeyID}); err != nil {
		t.Fatal(err)
	}
	if err = legacy.RecordQuotaHistory(ctx, []QuotaHistorySample{{Provider: "codex", CredentialID: strings.Repeat("a", 64), WindowID: "daily", RemainingPercent: 42, Source: "cpamc"}}, now); err != nil {
		t.Fatal(err)
	}
	if err = legacy.ReplaceProviderQuotaSnapshots(ctx, []ProviderQuotaSnapshot{{Provider: "codex", CredentialID: strings.Repeat("a", 64), Model: "m", Available: true, ObservedAt: now}}); err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.UpdatePriceBook(ctx, fixturePriceBook()); err != nil {
		t.Fatal(err)
	}
	if err = legacy.SaveImportCheckpoint(ctx, "old-import", "fixture", strings.Repeat("d", 64), 7, 2, []byte(`{"offset":7}`), false, [5]int64{10, 9, 8, 1, 1}); err != nil {
		t.Fatal(err)
	}
	if retained {
		if _, err = legacy.ApplyRetentionPolicy(ctx, time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC), 1); err != nil {
			t.Fatal(err)
		}
	}
	if err = legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO quota_history(provider,credential_id,window_id,observed_at_ns,remaining_percent,source) VALUES ('codex',?,'daily',?,19,'cpamc')", strings.Repeat("a", 64), now.Add(-120*24*time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE events SET import_batch_id='old-import' WHERE attempt_id=?", events[2].AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO pricing_catalog_rules(rule_id,provider,model,alias,input_per_million_nano,output_per_million_nano,cache_read_multiplier,cache_creation_multiplier,source) VALUES ('catalog-rule','codex','catalog-model','',1000000000,2000000000,'1','1','models.dev')"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO pricing_catalog_providers VALUES ('openai','OpenAI')"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT OR REPLACE INTO pricing_catalog_provenance VALUES (1,'models.dev',?, ?, ?, 0, '')", strings.Repeat("b", 64), now.UnixNano(), now.Add(time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT OR REPLACE INTO pricing_catalog_bindings VALUES (1,?)", strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	return config, events
}

func c4bSQLiteTotals(t *testing.T, path string) c4bTotals {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var out c4bTotals
	if err = db.QueryRow(`SELECT sum(attempts),sum(tokens),sum(money) FROM (SELECT count(*) attempts,coalesce(sum(total_tokens),0) tokens,coalesce(sum(known_cost_nano),0) money FROM events UNION ALL SELECT coalesce(sum(upstream_attempts),0),coalesce(sum(total_tokens),0),coalesce(sum(known_cost_nano),0) FROM rollups)`).Scan(&out.Attempts, &out.Tokens, &out.Money); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(DISTINCT proxy_request_id) FROM (SELECT proxy_request_id FROM events UNION ALL SELECT proxy_request_id FROM request_rollups)`).Scan(&out.Requests); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestC4bLegacyImportCurrentAndControl(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	config, events := c4bLegacyFixture(t, true)
	original, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	want := c4bSQLiteTotals(t, config.Path)
	s := openTestStore(t, config)
	if err = RunLegacyImport(ctx, s, config.Path); err != nil {
		t.Fatal(err)
	}
	if got := c4bVisible(t, s); got != want {
		t.Fatalf("import=%+v want=%+v", got, want)
	}
	source, err := sql.Open("sqlite", "file:"+config.Path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	for _, spec := range []struct{ table, columns string }{{"rollups", c4bRollupColumns}, {"request_rollups", c4bRequestColumns}} {
		var expected, got int64
		if err = source.QueryRow("SELECT count(*) FROM " + spec.table).Scan(&expected); err != nil {
			t.Fatal(err)
		}
		if err = s.db.Conn().QueryRow(ctx, "SELECT toInt64(count()) FROM "+s.table("analytics_"+spec.table)+" FINAL WHERE is_deleted=0").Scan(&got); err != nil {
			t.Fatal(err)
		}
		if expected != got {
			t.Fatalf("%s rows=%d want=%d", spec.table, got, expected)
		}
	}
	loaded, err := s.loadEvents(ctx, "attempt_id=?", events[2].AttemptID)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	if loaded[0].ImportBatchID != "old-import" {
		t.Fatalf("import ownership lost: %+v", loaded[0])
	}
	checkpoint, found, err := s.LoadImportCheckpoint(ctx, "old-import")
	if err != nil || !found || string(checkpoint) != `{"offset":7}` {
		t.Fatalf("source import checkpoint=%s found=%t err=%v", checkpoint, found, err)
	}
	if loaded[0].ProxyStatusCode == nil || *loaded[0].ProxyStatusCode != 503 || loaded[0].ProxyError == nil || *loaded[0].ProxyError != "recorded proxy failure" {
		t.Fatalf("patch lost: %+v", loaded[0])
	}
	var applied int64
	if err = source.QueryRow("SELECT applied_at_ns FROM schema_migrations WHERE version=18").Scan(&applied); err != nil {
		t.Fatal(err)
	}
	value, ok, err := s.metadata(ctx, "session_capture_since_ns")
	if err != nil || !ok || value != fmt.Sprint(applied) {
		t.Fatalf("session boundary=%s err=%v want=%d", value, err, applied)
	}
	for _, namespace := range []string{"pricing", "key_lifecycle", "provider_quota"} {
		values, err := s.loadControl(ctx, namespace)
		if err != nil || len(values) == 0 {
			t.Fatalf("%s=%v err=%v", namespace, values, err)
		}
	}
	snapshot, err := s.PricingSnapshot(ctx)
	if err != nil || len(snapshot.Rules) != 2 || len(snapshot.Catalog) != 1 || snapshot.Provenance.CatalogBindingsDigest != strings.Repeat("c", 64) || len(snapshot.Overrides) != 1 || snapshot.Provenance.Source != "management-api" {
		t.Fatalf("pricing=%+v err=%v", snapshot, err)
	}
	quotas, err := s.ProviderQuotaSnapshots(ctx)
	if err != nil || len(quotas) != 1 || !quotas[0].Available {
		t.Fatalf("quotas=%+v err=%v", quotas, err)
	}
	history, err := s.QuotaHistory(ctx, "codex", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil || len(history) != 1 || history[0].RemainingPercent != 42 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	var quotaCount int64
	if err = s.db.Conn().QueryRow(ctx, "SELECT toInt64(count()) FROM "+s.table("analytics_quota_history")).Scan(&quotaCount); err != nil || quotaCount != 2 {
		t.Fatalf("quota append table count=%d err=%v", quotaCount, err)
	}
	var sourceObserved int64
	if err = source.QueryRow("SELECT observed_at_ns FROM quota_history WHERE remaining_percent=42").Scan(&sourceObserved); err != nil {
		t.Fatal(err)
	}
	if history[0].ObservedAt.UnixNano() != sourceObserved {
		t.Fatalf("quota observed_at=%d want=%d", history[0].ObservedAt.UnixNano(), sourceObserved)
	}
	oldHistory, err := s.QuotaHistory(ctx, "codex", time.Now().Add(-121*24*time.Hour), time.Now().Add(-119*24*time.Hour))
	if err != nil || len(oldHistory) != 1 || oldHistory[0].RemainingPercent != 19 {
		t.Fatalf("old quota observation lost: %+v err=%v", oldHistory, err)
	}
	job, _, err := s.db.Job(ctx, c4bLegacyJob)
	if err != nil || job.State != "done" {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	var counts map[string]int64
	if err = json.Unmarshal([]byte(job.Detail), &counts); err != nil || counts["events"] != 1 || counts["quota_history"] != 2 {
		t.Fatalf("counts=%v err=%v", counts, err)
	}
	// Done does not inspect even a missing or malformed path.
	if err = RunLegacyImport(ctx, s, "/does/not/exist"); err != nil {
		t.Fatal(err)
	}
	if got := c4bVisible(t, s); got != want {
		t.Fatalf("done replay=%+v want=%+v", got, want)
	}
	after, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, after) {
		t.Fatal("legacy source bytes changed")
	}
}

func TestC4bLegacyImportCrashReplay(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"events-written", "events-checkpoint", "rollups-written", "request_rollups-written", "control-written", "published"} {
		t.Run(step, func(t *testing.T) {
			config, _ := c4bLegacyFixture(t, false)
			want := c4bSQLiteTotals(t, config.Path)
			s := openTestStore(t, config)
			for _, table := range []string{"analytics_events", "analytics_rollups", "analytics_request_rollups", "analytics_quota_history"} {
				if err := s.db.Conn().Exec(context.Background(), "SYSTEM STOP MERGES "+s.table(table)); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			ctx = context.WithValue(ctx, c4bStepKey{}, func(name string) error {
				if name == "legacy:"+step {
					cancel()
					return ctx.Err()
				}
				return nil
			})
			if err := RunLegacyImport(ctx, s, config.Path); !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v", err)
			}
			job, _, err := s.db.Job(context.Background(), c4bLegacyJob)
			if err != nil || job.State != "failed" {
				t.Fatalf("failed job=%+v err=%v", job, err)
			}
			if err = RunLegacyImport(context.Background(), s, config.Path); err != nil {
				t.Fatal(err)
			}
			if got := c4bVisible(t, s); got != want {
				t.Fatalf("retry=%+v want=%+v", got, want)
			}
			var quotaCount int64
			if err = s.db.Conn().QueryRow(context.Background(), "SELECT toInt64(count()) FROM "+s.table("analytics_quota_history")).Scan(&quotaCount); err != nil || quotaCount != 2 {
				t.Fatalf("quota replay count=%d err=%v", quotaCount, err)
			}
			var events int64
			if err = s.db.Conn().QueryRow(context.Background(), "SELECT toInt64(count()) FROM "+s.table("analytics_events")+" FINAL WHERE is_deleted=0").Scan(&events); err != nil {
				t.Fatal(err)
			}
			if events != want.Attempts {
				t.Fatalf("duplicate events=%d want=%d", events, want.Attempts)
			}
		})
	}
}

func TestC4bLegacyMissingAndFailedRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	if err := s.db.SetJob(ctx, c4bLegacyJob, "pending", ""); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "bad.db")
	if err := os.WriteFile(bad, []byte("not a SQLite DB"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RunLegacyImport(ctx, s, bad); err == nil {
		t.Fatal("accepted corrupt source")
	}
	job, _, err := s.db.Job(ctx, c4bLegacyJob)
	if err != nil || job.State != "failed" {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if err = RunLegacyImport(ctx, s, filepath.Join(t.TempDir(), "missing.db")); err != nil {
		t.Fatal(err)
	}
	job, _, err = s.db.Job(ctx, c4bLegacyJob)
	if err != nil || job.State != "done" {
		t.Fatalf("job=%+v err=%v", job, err)
	}
}

func TestC4bLegacyOldSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	config, _ := c4bLegacyFixture(t, false)
	seed := config.Path
	oldPath := filepath.Join(filepath.Dir(seed), "schema-v1.db")
	db, err := sql.Open("sqlite", "file:"+oldPath)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile(filepath.Join("..", "store", "migrations", "001_schema_v1.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,name TEXT NOT NULL,checksum TEXT NOT NULL,applied_at_ns INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	checksum := sha256.Sum256(migration)
	if _, err = db.Exec("INSERT INTO schema_migrations VALUES(1,?,?,?)", "001_schema_v1.sql", fmt.Sprintf("%x", checksum), time.Now().Add(-24*time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("ATTACH DATABASE ? AS seed", seed); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO analytics_metadata SELECT * FROM seed.analytics_metadata"); err != nil {
		t.Fatal(err)
	}
	columns, err := db.Query("PRAGMA table_info(events)")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for columns.Next() {
		var cid, notnull, pk int
		var name, kind string
		var defaultValue any
		if err = columns.Scan(&cid, &name, &kind, &notnull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err = columns.Err(); err != nil {
		t.Fatal(err)
	}
	_ = columns.Close()
	list := strings.Join(names, ",")
	if _, err = db.Exec("INSERT INTO events(" + list + ") SELECT " + list + " FROM seed.events"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	config.Path = oldPath
	s := openTestStore(t, config)
	before := time.Now().UTC().UnixNano()
	if err = RunLegacyImport(ctx, s, oldPath); err != nil {
		t.Fatal(err)
	}
	want := c4bSQLiteTotals(t, oldPath)
	if got := c4bVisible(t, s); got != want {
		t.Fatalf("old schema=%+v want=%+v", got, want)
	}
	boundary, ok, err := s.metadata(ctx, "session_capture_since_ns")
	if err != nil || !ok {
		t.Fatalf("boundary=%s err=%v", boundary, err)
	}
	var ns int64
	if _, err = fmt.Sscan(boundary, &ns); err != nil || ns < before || ns > time.Now().UnixNano() {
		t.Fatalf("old source boundary=%s err=%v", boundary, err)
	}
	after, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, after) {
		t.Fatal("old schema source was migrated in place")
	}
}

func TestC4bLegacyResumesAcrossEventChunks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	config := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: fixturePriceBook()}
	legacy, err := store.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	fixture := loadFixtureEvents(t)[0]
	events := make([]model.Event, 1003)
	for i := range events {
		events[i] = fixture
		events[i].AttemptID = fmt.Sprintf("%032x", i+1)
		events[i].ProxyRequestID = fmt.Sprintf("%032x", i/2+1)
	}
	if err = legacy.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	if err = legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, config)
	chunks := 0
	interrupted, cancel := context.WithCancel(ctx)
	interrupted = context.WithValue(interrupted, c4bStepKey{}, func(step string) error {
		if step == "legacy:events-written" {
			chunks++
			if chunks == 2 {
				cancel()
				return context.Canceled
			}
		}
		return nil
	})
	if err = RunLegacyImport(interrupted, s, config.Path); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	progress, err := s.loadControl(ctx, "retention")
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint c4bLegacyProgress
	if err = json.Unmarshal(progress[c4bLegacyJob], &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.Counts["events"] != 1000 || checkpoint.After != events[999].AttemptID {
		t.Fatalf("checkpoint=%+v", checkpoint)
	}
	// Simulate restart after an ungraceful shutdown that left the job running.
	if err = s.db.SetJob(ctx, c4bLegacyJob, "running", ""); err != nil {
		t.Fatal(err)
	}
	if err = RunLegacyImport(ctx, s, config.Path); err != nil {
		t.Fatal(err)
	}
	want := c4bSQLiteTotals(t, config.Path)
	if got := c4bVisible(t, s); got != want {
		t.Fatalf("resumed=%+v want=%+v", got, want)
	}
	job, _, err := s.db.Job(ctx, c4bLegacyJob)
	if err != nil {
		t.Fatal(err)
	}
	var counts map[string]int64
	if err = json.Unmarshal([]byte(job.Detail), &counts); err != nil || counts["events"] != 1003 {
		t.Fatalf("counts=%v err=%v", counts, err)
	}
}

func TestC4bLegacyCompletesPartialRawCutoffOnCopy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	config, _ := c4bLegacyFixture(t, false)
	legacy, err := store.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 8, 10, 4, 30, 0, 0, time.UTC)
	result, err := legacy.ApplyRetention(ctx, cutoff, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedRows != 0 {
		t.Fatalf("source partial hour unexpectedly removed: %+v", result)
	}
	if err = legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	want := c4bSQLiteTotals(t, config.Path)
	s := openTestStore(t, config)
	if err = RunLegacyImport(ctx, s, config.Path); err != nil {
		t.Fatal(err)
	}
	if got := c4bVisible(t, s); got != want {
		t.Fatalf("partial-cutoff import=%+v want=%+v", got, want)
	}
	after, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("partial buckets completed in original source")
	}
}

func TestC4bLegacySnapshotsReadOnlyWAL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	config := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: fixturePriceBook()}
	legacy, err := store.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = legacy.Close(ctx) }()
	events := loadFixtureEvents(t)[:3]
	if err = legacy.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	before, err := c4bSourceFingerprint(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	want := c4bSQLiteTotals(t, config.Path)
	s := openTestStore(t, config)
	if err = RunLegacyImport(ctx, s, config.Path); err != nil {
		t.Fatal(err)
	}
	if got := c4bVisible(t, s); got != want {
		t.Fatalf("WAL snapshot=%+v want=%+v", got, want)
	}
	after, err := c4bSourceFingerprint(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("source main database or WAL bytes changed")
	}
}

func TestC4bLegacySessionBoundaryReadParity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	config, events := c4bLegacyFixture(t, false)
	db, err := sql.Open("sqlite", "file:"+config.Path)
	if err != nil {
		t.Fatal(err)
	}
	since := events[0].RequestedAt.Add(time.Hour)
	if _, err = db.Exec("UPDATE schema_migrations SET applied_at_ns=? WHERE version=18", since.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, config)
	if err = RunLegacyImport(ctx, s, config.Path); err != nil {
		t.Fatal(err)
	}
	q := model.Query{SchemaVersion: 1, Operation: model.OperationEvents, Start: events[0].RequestedAt.Add(-time.Hour), End: events[2].RequestedAt.Add(time.Hour), TimeZone: "UTC", PageSize: 100}
	page, err := s.Events(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 3 {
		t.Fatalf("events=%+v", page)
	}
	for _, event := range page.Events {
		want := event.AttemptID == events[1].AttemptID
		if event.SessionCaptured != want {
			t.Fatalf("attempt=%s captured=%t want=%t", event.AttemptID, event.SessionCaptured, want)
		}
	}
}
