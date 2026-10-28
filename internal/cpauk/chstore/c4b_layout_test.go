package chstore

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestC4bMonthlyPruningAndGlobalAttemptLookup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	base := loadFixtureEvents(t)[0]
	const count = 20000
	for offset := 0; offset < count; offset += 5000 {
		events := make([]model.Event, 5000)
		for i := range events {
			events[i] = base
			events[i].AttemptID = fmt.Sprintf("%032x", offset+i+1)
			events[i].RequestedAt = start.Add(time.Duration(offset+i) * time.Minute * 4)
		}
		if err := s.WriteBatch(ctx, events); err != nil {
			t.Fatal(err)
		}
	}
	target := fmt.Sprintf("%032x", count)
	var plan strings.Builder
	query := "EXPLAIN indexes=1 SELECT requested_at_ns FROM " + s.table("analytics_events") + " FINAL PREWHERE attempt_id=? WHERE is_deleted=0 SETTINGS use_skip_indexes_if_final=1"
	rows, err := s.db.Conn().Query(ctx, query, target)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "attempt_id_bloom") {
		t.Fatalf("lookup did not use bloom index:\n%s", plan.String())
	}
	queryID := fmt.Sprintf("c4b-pruning-%d", time.Now().UnixNano())
	queryCtx := ch.Context(ctx, ch.WithQueryID(queryID))
	var matched uint64
	if err = s.db.Conn().QueryRow(queryCtx, "SELECT count() FROM "+s.table("analytics_events")+" FINAL WHERE is_deleted=0 AND requested_at_ns>=? AND requested_at_ns<?", start.AddDate(0, 0, 35).UnixNano(), start.AddDate(0, 0, 36).UnixNano()).Scan(&matched); err != nil {
		t.Fatal(err)
	}
	if matched != 360 {
		t.Fatalf("one day count=%d", matched)
	}
	if err = s.db.Conn().Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatal(err)
	}
	var scanned uint64
	if err = s.db.Conn().QueryRow(ctx, "SELECT sum(read_rows) FROM system.query_log WHERE type='QueryFinish' AND query_id=?", queryID).Scan(&scanned); err != nil {
		t.Fatal(err)
	}
	if scanned >= count || scanned < matched {
		t.Fatalf("one day scanned=%d total=%d matched=%d", scanned, count, matched)
	}
	t.Logf("one-day scan: read_rows=%d total_rows=%d matches=%d", scanned, count, matched)
	q := model.Query{SchemaVersion: 1, Operation: model.OperationEvents, Start: start, End: start.AddDate(0, 0, 60), TimeZone: "UTC", PageSize: 100}
	event, found, err := s.EventByAttemptID(ctx, target, q)
	if err != nil || !found || event.AttemptID != target {
		t.Fatalf("lookup found=%t err=%v event=%+v", found, err, event)
	}
	// A replay naming a different month still finds the original globally.
	event.RequestedAt = event.RequestedAt.AddDate(0, 3, 0)
	if err = s.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	var duplicates uint64
	if err = s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table("analytics_events")+" FINAL PREWHERE attempt_id=? WHERE is_deleted=0", target).Scan(&duplicates); err != nil || duplicates != 1 {
		t.Fatalf("global duplicate count=%d err=%v", duplicates, err)
	}
}

func TestC4bBulkRetentionDropsMonthsAndKeepsReceipts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	base := loadFixtureEvents(t)[0]
	events := []model.Event{base, base, base, base}
	for i := range events {
		events[i].AttemptID = fmt.Sprintf("%032x", 50001+i)
		events[i].ProxyRequestID = base.ProxyRequestID
	}
	events[0].RequestedAt = time.Date(2026, 6, 12, 4, 5, 0, 0, time.UTC)
	events[1].RequestedAt = time.Date(2026, 7, 12, 4, 5, 0, 0, time.UTC)
	events[2].RequestedAt = time.Date(2026, 8, 12, 4, 5, 0, 0, time.UTC)
	events[3].RequestedAt = time.Date(2026, 8, 16, 4, 5, 0, 0, time.UTC)
	if err := s.WriteBatchWithPatches(ctx, events, []model.ProxyResponsePatch{{ProxyRequestID: base.ProxyRequestID, StatusCode: 502, RespondedAt: events[3].RequestedAt.Add(time.Second)}}); err != nil {
		t.Fatal(err)
	}
	want := c4bVisible(t, s)
	cutoff := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	runID := fmt.Sprintf("c4b-bulk-cleanup-%d", time.Now().UnixNano())
	retentionCtx := ch.Context(ctx, ch.WithQueryID(runID))
	if result, err := s.ApplyRetention(retentionCtx, cutoff, 1); err != nil || result.DeletedRows != 3 {
		t.Fatalf("retention=%+v err=%v", result, err)
	}
	if err := s.db.Conn().Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatal(err)
	}
	var boundaryDeletes, monthDrops, outcomeUpdates uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT countIf(startsWith(query,?)),countIf(startsWith(query,?)),countIf(startsWith(query,?)) FROM system.query_log WHERE type='QueryFinish' AND query_id=?", "ALTER TABLE "+s.table("analytics_events")+" DELETE WHERE requested_at_ns>=", "ALTER TABLE "+s.table("analytics_events")+" DROP PARTITION ID", "ALTER TABLE "+s.table("analytics_outcomes")+" UPDATE attempt_ids=", runID).Scan(&boundaryDeletes, &monthDrops, &outcomeUpdates); err != nil {
		t.Fatal(err)
	}
	if boundaryDeletes != 1 || monthDrops != 2 || outcomeUpdates != 1 {
		t.Fatalf("cleanup queries: boundary_deletes=%d month_drops=%d outcome_updates=%d", boundaryDeletes, monthDrops, outcomeUpdates)
	}
	if got := c4bVisible(t, s); got != want {
		t.Fatalf("totals=%+v want=%+v", got, want)
	}
	var oldParts uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM system.parts WHERE active AND database=? AND table='analytics_events' AND partition IN ('202606','202607')", s.db.Database()).Scan(&oldParts); err != nil || oldParts != 0 {
		t.Fatalf("old month parts=%d err=%v", oldParts, err)
	}
	survivor, err := s.loadEvents(ctx, "attempt_id=?", events[3].AttemptID)
	if err != nil || len(survivor) != 1 || survivor[0].ProxyStatusCode == nil || *survivor[0].ProxyStatusCode != 502 {
		t.Fatalf("surviving shared patch=%+v err=%v", survivor, err)
	}
	late := events[0]
	late.AttemptID = fmt.Sprintf("%032x", 60001)
	if inserted, err := s.WriteImportBatch(ctx, []model.Event{late}, "old-month"); err != nil || inserted != 1 {
		t.Fatalf("late=%d err=%v", inserted, err)
	}
	if _, err := s.ApplyRetention(ctx, cutoff, 1); err != nil {
		t.Fatal(err)
	}
	if inserted, err := s.WriteImportBatch(ctx, []model.Event{late}, "old-month"); err != nil || inserted != 0 {
		t.Fatalf("receipt lost after month cleanup: inserted=%d err=%v", inserted, err)
	}
	var receipts uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM system.parts WHERE active AND database=? AND table='analytics_events' AND partition='0'", s.db.Database()).Scan(&receipts); err != nil || receipts == 0 {
		t.Fatalf("receipt partition count=%d err=%v", receipts, err)
	}
}
