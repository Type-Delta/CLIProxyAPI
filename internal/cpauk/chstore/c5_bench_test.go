package chstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	cpach "github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// TestC5ReadBenchmark is deliberately opt-in: it creates millions of rows and
// runs retention against an isolated database. CPA_CHSTORE_BENCH_ROWS and
// CPA_CHSTORE_BENCH_DAYS permit small harness smoke tests, not gate results.
func TestC5ReadBenchmark(t *testing.T) {
	if os.Getenv("CPA_CHSTORE_BENCH") != "1" {
		t.Skip("set CPA_CHSTORE_BENCH=1")
	}
	ctx := context.Background()
	number := func(name string, fallback int) int {
		v := os.Getenv(name)
		if v == "" {
			return fallback
		}
		n, e := strconv.Atoi(v)
		if e != nil || n < 1 {
			t.Fatalf("invalid %s", name)
		}
		return n
	}
	count, days := number("CPA_CHSTORE_BENCH_ROWS", 5000000), number("CPA_CHSTORE_BENCH_DAYS", 400)
	rate := model.NanoUSD(1_000_000_000)
	book := aggregate.PriceBook{}
	for i := 0; i < 40; i++ {
		book.Rules = append(book.Rules, aggregate.PricingRule{ID: fmt.Sprintf("bench-%02d", i), Model: fmt.Sprintf("model-%02d", i), InputPerMillion: &rate, OutputPerMillion: &rate, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "benchmark"})
	}
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 24 << 30, PriceBook: book})
	end := time.Now().UTC().Truncate(24 * time.Hour)
	start := end.AddDate(0, 0, -days)
	keys := make([]string, 20)
	for i := range keys {
		keys[i] = model.KeyID(fmt.Sprintf("benchmark-key-%d", i))
	}
	credentials := make([]string, 200)
	sessions := make([]string, 10000)
	credentialAlgorithm := model.CredentialIDAlgorithm
	for i := range credentials {
		credentials[i] = fmt.Sprintf("%064x", i+1)
	}
	for i := range sessions {
		sessions[i] = fmt.Sprintf("%064x", i+1)
	}
	t.Logf("database=%s generating=%d days=%d", s.db.Database(), count, days)
	for base := 0; base < count; base += 5000 {
		size := min(5000, count-base)
		events := make([]model.Event, size)
		patches := make([]model.ProxyResponsePatch, 0, size/3)
		for j := range events {
			i := base + j
			request := i
			if i%20 == 19 {
				request = i - 1
			}
			at := start.Add(time.Duration(int64(end.Sub(start)) / int64(count) * int64(i)))
			status := 200
			success := i%100 >= 3
			var class *string
			if !success {
				status = 429
				value := "rate_limit"
				class = &value
			}
			events[j] = model.Event{SchemaVersion: model.EventSchemaVersion, AttemptID: fmt.Sprintf("%032x", i+1), ProxyRequestID: fmt.Sprintf("%032x", request+1), RequestIDQuality: model.RequestIDObserved, KeyID: keys[(request/7)%20], RequestedAt: at, Provider: fmt.Sprintf("provider-%d", (request/11)%8), ExecutorType: "benchmark", Model: fmt.Sprintf("model-%02d", (request/13)%40), EndpointClass: "responses", CredentialID: &credentials[(request/17)%200], CredentialIDAlgorithm: &credentialAlgorithm, SessionDigest: &sessions[(request/19)%10000], Succeeded: success, UpstreamStatusCode: &status, ErrorClass: class, LatencyMS: 100 + int64(i%1500), Generated: true, Tokens: model.TokenUsage{Input: 1000, Output: 200, Total: 1200, Schema: model.AccountingSchemaNormalizedV1, Quality: model.TokenQualityExact}}
			if request == i && i%10 < 3 {
				patches = append(patches, model.ProxyResponsePatch{ProxyRequestID: events[j].ProxyRequestID, StatusCode: 200, RespondedAt: at.Add(time.Second)})
			}
		}
		if err := s.WriteBatchWithPatches(ctx, events, patches); err != nil {
			t.Fatalf("write %d: %v", base, err)
		}
		if base%100000 == 0 {
			t.Logf("loaded %d/%d", base+size, count)
		}
	}
	cutoff := end.AddDate(0, 0, -min(100, days/4))
	fixtureCutoff := cutoff.Add(-24 * time.Hour)
	t.Logf("bulk fixture retention through %s", fixtureCutoff)
	var fixtureRetention RetentionResult
	if err := s.c4bPromote(ctx, fixtureCutoff, false, &fixtureRetention); err != nil {
		t.Fatal(err)
	}
	if err := s.c4bCleanup(ctx, s.retentionCutoff.UnixNano(), false); err != nil {
		t.Fatal(err)
	}
	t.Logf("bulk_fixture_promoted_events=%d cutoff=%s", fixtureRetention.DeletedRows, s.retentionCutoff)
	t.Logf("applying real one-day retention through %s", cutoff)
	retentionStarted := time.Now()
	retention, err := s.ApplyRetention(ctx, cutoff, 10000)
	if err != nil {
		t.Fatal(err)
	}
	elapsedRetention := time.Since(retentionStarted)
	var retainedBuckets uint64
	if err = s.db.Conn().QueryRow(ctx, "SELECT uniqExact(bucket_start_ns) FROM "+s.table("analytics_rollups")+" FINAL WHERE grain='hourly' AND is_deleted=0 AND "+s.c4bPublishedPredicate()).Scan(&retainedBuckets); err != nil {
		t.Fatal(err)
	}
	t.Logf("one_day_retention_elapsed=%s promoted_events=%d total_hourly_buckets=%d", elapsedRetention, retention.DeletedRows, retainedBuckets)
	for _, table := range []string{"analytics_events", "analytics_outcomes", "analytics_rollups", "analytics_request_rollups", "analytics_retention_heads"} {
		if err := s.db.Conn().Exec(ctx, "OPTIMIZE TABLE "+s.table(table)+" FINAL"); err != nil {
			t.Fatal(err)
		}
	}
	var bytes uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT sum(bytes_on_disk) FROM system.parts WHERE active AND database=? AND startsWith(table,'analytics_')", s.db.Database()).Scan(&bytes); err != nil {
		t.Fatal(err)
	}
	t.Logf("analytics_bytes_on_disk=%d", bytes)
	if clone := os.Getenv("CPA_CHSTORE_BENCH_SAVE_DB"); clone != "" {
		if !strings.HasPrefix(clone, "cpa_test_") {
			t.Fatal("benchmark copy must use cpa_test_ prefix")
		}
		if err := s.db.Conn().Exec(ctx, "CREATE DATABASE `"+clone+"`"); err != nil {
			t.Fatal(err)
		}
		rows, err := s.db.Conn().Query(ctx, "SELECT name FROM system.tables WHERE database=? AND engine LIKE '%MergeTree'", s.db.Database())
		if err != nil {
			t.Fatal(err)
		}
		var tables []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			tables = append(tables, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		for _, name := range tables {
			dest := "`" + clone + "`.`" + name + "`"
			if err := s.db.Conn().Exec(ctx, "CREATE TABLE "+dest+" AS "+s.table(name)); err != nil {
				t.Fatal(err)
			}
			if err := s.db.Conn().Exec(ctx, "ALTER TABLE "+dest+" ATTACH PARTITION ALL FROM "+s.table(name)); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("saved_benchmark_database=%s", clone)
	}

	type measure struct {
		name, id string
		duration time.Duration
		err      error
	}
	var measures []measure
	type reader struct {
		name      string
		op        model.Operation
		configure func(*model.Query)
		call      func(context.Context, model.Query) error
	}
	readers := []reader{
		{"summary", model.OperationSummary, nil, func(c context.Context, q model.Query) error { _, e := s.Summary(c, q); return e }},
		{"timeseries_day", model.OperationTimeseries, func(q *model.Query) { q.BucketWidth = "1d" }, func(c context.Context, q model.Query) error { _, e := s.Timeseries(c, q); return e }},
		{"timeseries_hour", model.OperationTimeseries, func(q *model.Query) { q.BucketWidth = "1h" }, func(c context.Context, q model.Query) error { _, e := s.Timeseries(c, q); return e }},
		{"leaderboard", model.OperationLeaderboard, func(q *model.Query) { q.PageSize = 50; q.SortBy = model.LeaderboardSortTokens }, func(c context.Context, q model.Query) error { _, e := s.Leaderboard(c, q); return e }},
		{"model_leaderboard", model.OperationModelLeaderboard, nil, func(c context.Context, q model.Query) error { _, e := s.ModelLeaderboard(c, q); return e }},
		{"model_cost_series", model.OperationModelCostSeries, func(q *model.Query) {
			q.Filters = map[string]json.RawMessage{"model": json.RawMessage(`["model-00"]`)}
			q.BucketWidth = "1d"
		}, func(c context.Context, q model.Query) error {
			result, err := s.ModelCostSeries(c, q)
			if err == nil && result.Partial {
				return fmt.Errorf("model cost series returned partial coverage")
			}
			return err
		}},
		{"activity", model.OperationActivity, func(q *model.Query) { q.Window = "year" }, func(c context.Context, q model.Query) error { _, e := s.Activity(c, q); return e }},
		{"analysis", model.OperationAnalysis, func(q *model.Query) { q.BucketWidth = "1d" }, func(c context.Context, q model.Query) error {
			result, err := s.Analysis(c, q)
			if err != nil {
				return err
			}
			if os.Getenv("CPA_CHSTORE_BENCH_ANALYSIS_ONLY") != "1" && (result.SeriesByCategory.Meta.Partial || result.ModelByTime.Meta.Partial || result.Latency.Meta.Partial || result.CostComponents.Meta.Partial || result.KeyModelMatrix.Meta.Partial) {
				return fmt.Errorf("analysis returned a partial section")
			}
			return nil
		}},
		{"events_first", model.OperationEvents, func(q *model.Query) { q.PageSize = 50 }, func(c context.Context, q model.Query) error { _, e := s.Events(c, q); return e }},
		{"events_deep", model.OperationEvents, func(q *model.Query) { q.PageSize = 50 }, func(c context.Context, q model.Query) error {
			selection, err := q.SelectionDigest()
			if err != nil {
				return err
			}
			at := q.Start.Add(q.End.Sub(q.Start) / 2).UTC()
			q.Cursor, err = s.config.CursorCodec.Encode(model.Cursor{Version: 1, Operation: model.OperationEvents, Selection: selection, RequestedAt: &at, AttemptID: "00000000000000000000000000000000"})
			if err != nil {
				return err
			}
			_, err = s.Events(c, q)
			return err
		}},
		{"event_by_id", model.OperationEvents, func(q *model.Query) { q.PageSize = 50 }, func(c context.Context, q model.Query) error {
			i := count - 1
			if len(q.KeyIDs) != 0 {
				for i > 0 {
					request := i
					if i%20 == 19 {
						request--
					}
					if (request/7)%20 == 0 {
						break
					}
					i--
				}
			}
			_, found, err := s.EventByAttemptID(c, fmt.Sprintf("%032x", i+1), q)
			if err == nil && !found {
				return fmt.Errorf("benchmark event not found")
			}
			return err
		}},
	}
	for _, dimension := range []string{"provider", "model", "credential", "key", "endpoint", "failure", "latency", "cache", "service_tier", "source"} {
		d := dimension
		readers = append(readers, reader{"dimensions_" + d, model.OperationDimensions, func(q *model.Query) { q.Dimension = d; q.PageSize = 50 }, func(c context.Context, q model.Query) error { _, e := s.Dimensions(c, q); return e }})
	}
	for _, d := range []int{1, 7, 30, 365} {
		if os.Getenv("CPA_CHSTORE_BENCH_ANALYSIS_ONLY") == "1" && d != 30 && d != 365 {
			continue
		}
		for _, filtered := range []bool{false, true} {
			for _, r := range readers {
				if os.Getenv("CPA_CHSTORE_BENCH_ANALYSIS_ONLY") == "1" && r.name != "analysis" {
					continue
				}
				q := model.Query{SchemaVersion: 2, Operation: r.op, Start: end.AddDate(0, 0, -d), End: end, TimeZone: "UTC"}
				if filtered {
					q.KeyIDs = []string{keys[0]}
				}
				if r.configure != nil {
					r.configure(&q)
				}
				if r.name == "activity" {
					q.Window = map[int]string{1: "day", 7: "week", 30: "month", 365: "year"}[d]
				}
				name := fmt.Sprintf("%s/%dd/key=%t", r.name, d, filtered)
				for run := 0; run < 5; run++ {
					id := fmt.Sprintf("c5-%d-%d", time.Now().UnixNano(), run)
					c := ch.Context(ctx, ch.WithQueryID(id))
					before := time.Now()
					err := r.call(c, q)
					measures = append(measures, measure{name, id, time.Since(before), err})
					if err != nil {
						break
					}
				}
			}
		}
	}
	if err := s.db.Conn().Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(measures); {
		j := i + 1
		for j < len(measures) && measures[j].name == measures[i].name {
			j++
		}
		group := measures[i:j]
		if group[0].err != nil {
			t.Logf("%s ERROR %v", group[0].name, group[0].err)
			i = j
			continue
		}
		durations := make([]time.Duration, 0, len(group))
		var rows, bytes uint64
		for _, m := range group {
			durations = append(durations, m.duration)
			var rr, bb uint64
			if err := s.db.Conn().QueryRow(ctx, "SELECT sum(read_rows),sum(read_bytes) FROM system.query_log WHERE type='QueryFinish' AND query_id=?", m.id).Scan(&rr, &bb); err != nil {
				t.Fatal(err)
			}
			rows += rr
			bytes += bb
		}
		sort.Slice(durations, func(a, b int) bool { return durations[a] < durations[b] })
		t.Logf("%s p50=%s p95=%s rows_read_mean=%d read_bytes_mean=%d", group[0].name, durations[len(durations)/2], durations[len(durations)-1], rows/uint64(len(group)), bytes/uint64(len(group)))
		i = j
	}
}

// Compare the benchmark's bulk fixture with the public maintenance operation,
// including the two fact tables, publication heads, cutoffs and source cleanup.
func TestC5BulkRetentionParity(t *testing.T) {
	if os.Getenv("CPA_CHSTORE_BENCH") != "1" {
		t.Skip("set CPA_CHSTORE_BENCH=1")
	}
	ctx := context.Background()
	fixture, real := openTestStore(t), openTestStore(t)
	events := syntheticDifferentialEvents(t)
	patches := []model.ProxyResponsePatch{{ProxyRequestID: events[0].ProxyRequestID, StatusCode: 200, RespondedAt: events[0].RequestedAt.Add(time.Second)}, {ProxyRequestID: events[len(events)-1].ProxyRequestID, StatusCode: 200, RespondedAt: events[len(events)-1].RequestedAt.Add(time.Second)}}
	for _, s := range []*Store{fixture, real} {
		if err := s.WriteBatchWithPatches(ctx, events, patches); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := events[0].RequestedAt.Truncate(time.Hour).Add(72 * time.Hour)
	var result RetentionResult
	if err := fixture.c4bPromote(ctx, cutoff, false, &result); err != nil {
		t.Fatal(err)
	}
	if err := fixture.c4bCleanup(ctx, fixture.retentionCutoff.UnixNano(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := real.ApplyRetention(ctx, cutoff, 10000); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct{ table, cols string }{
		{"analytics_rollups", c4bRollupColumns}, {"analytics_request_rollups", c4bRequestColumns},
		{"analytics_retention_heads", "grain,bucket_start_ns,import_batch_id"},
		{"analytics_events", "attempt_id,requested_at_ns"},
		{"analytics_outcomes", "proxy_request_id,attempt_ids,status_code,error,responded_at_ns"},
	} {
		for _, pair := range [][2]*Store{{fixture, real}, {real, fixture}} {
			var differences uint64
			final := " FINAL"
			if spec.table == "analytics_outcomes" {
				final = ""
			}
			query := "SELECT count() FROM (SELECT " + spec.cols + " FROM " + pair[0].table(spec.table) + final + " EXCEPT SELECT " + spec.cols + " FROM " + pair[1].table(spec.table) + final + ")"
			if err := fixture.db.Conn().QueryRow(ctx, query).Scan(&differences); err != nil {
				t.Fatalf("compare %s: %v", spec.table, err)
			}
			if differences != 0 {
				t.Fatalf("%s differs: %d", spec.table, differences)
			}
		}
	}
	a, err := fixture.readCutoffs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := real.readCutoffs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("cutoffs differ: %+v vs %+v", a, b)
	}
}

// TestC5Followup reads a separately copied benchmark database, which the caller
// must create and clean up. It measures partial responses without hiding them.
func TestC5Followup(t *testing.T) {
	dsn, dbname := os.Getenv("CPA_CHSTORE_FOLLOWUP_DSN"), os.Getenv("CPA_CHSTORE_FOLLOWUP_DB")
	if os.Getenv("CPA_CHSTORE_BENCH") != "1" || dsn == "" || dbname == "" {
		t.Skip("requires explicit benchmark copy")
	}
	ctx := context.Background()
	h := cpach.NewHandle(cpach.Config{DSN: dsn, Database: dbname})
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	db, err := h.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rate := model.NanoUSD(1_000_000_000)
	book := aggregate.PriceBook{}
	for i := 0; i < 40; i++ {
		book.Rules = append(book.Rules, aggregate.PricingRule{ID: fmt.Sprintf("bench-%02d", i), Model: fmt.Sprintf("model-%02d", i), InputPerMillion: &rate, OutputPerMillion: &rate, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "benchmark"})
	}
	codec, err := model.NewCursorCodec([]byte("benchmark-cursor-key-32-bytes!!!"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db, config: Config{PriceBook: book, RetentionTimeZone: "UTC", CursorCodec: codec}}
	bounds, err := s.readCutoffs(ctx)
	if err != nil || !bounds.rawSet {
		t.Fatalf("cutoffs %+v: %v", bounds, err)
	}
	end := time.Unix(0, bounds.raw).UTC().AddDate(0, 0, 100)
	for _, filtered := range []bool{false, true} {
		for _, days := range []int{30, 365} {
			for _, operation := range []model.Operation{model.OperationAnalysis} {
				q := model.Query{SchemaVersion: 2, Operation: operation, Start: end.AddDate(0, 0, -days), End: end, TimeZone: "UTC", BucketWidth: "1h"}
				if filtered {
					q.KeyIDs = []string{model.KeyID("benchmark-key-0")}
				}
				if operation == model.OperationAnalysis {
					q.BucketWidth = "1d"
				}
				var durations []time.Duration
				var ids []string
				sections := ""
				for i := 0; i < 5; i++ {
					id := fmt.Sprintf("bm-followup-%d-%d", time.Now().UnixNano(), i)
					c := ch.Context(ctx, ch.WithQueryID(id))
					before := time.Now()
					if operation == model.OperationAnalysis {
						x, e := s.Analysis(c, q)
						err = e
						if e == nil {
							sections = fmt.Sprintf("series=%t models=%t latency=%t costs=%t matrix=%t", x.SeriesByCategory.Meta.Partial, x.ModelByTime.Meta.Partial, x.Latency.Meta.Partial, x.CostComponents.Meta.Partial, x.KeyModelMatrix.Meta.Partial)
						}
					} else {
						_, err = s.Timeseries(c, q)
					}
					durations = append(durations, time.Since(before))
					ids = append(ids, id)
					if err != nil {
						t.Fatalf("%s: %v", operation, err)
					}
				}
				if err := db.Conn().Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
					t.Fatal(err)
				}
				var rows, bytes uint64
				for _, id := range ids {
					var r, b uint64
					if err := db.Conn().QueryRow(ctx, "SELECT sum(read_rows),sum(read_bytes) FROM system.query_log WHERE type='QueryFinish' AND query_id=?", id).Scan(&r, &b); err != nil {
						t.Fatal(err)
					}
					rows += r
					bytes += b
				}
				if os.Getenv("CPA_CHSTORE_BENCH_PROFILE") == "1" {
					rows, err := db.Conn().Query(ctx, "SELECT substring(query,1,120),sum(read_rows),sum(query_duration_ms) FROM system.query_log WHERE type='QueryFinish' AND query_id IN (?) GROUP BY normalized_query_hash,substring(query,1,120) ORDER BY sum(query_duration_ms) DESC", ids)
					if err != nil {
						t.Fatal(err)
					}
					for rows.Next() {
						var sql string
						var reads, millis uint64
						if err := rows.Scan(&sql, &reads, &millis); err != nil {
							t.Fatal(err)
						}
						t.Logf("profile %dd key=%t total_ms=%d rows=%d query=%s", days, filtered, millis, reads, sql)
					}
					if err := rows.Err(); err != nil {
						t.Fatal(err)
					}
					_ = rows.Close()
				}
				sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
				t.Logf("%s/%dd/key=%t p50=%s p95=%s rows_read_mean=%d read_bytes_mean=%d partial: %s", operation, days, filtered, durations[2], durations[4], rows/5, bytes/5, sections)
			}
		}
	}
}
