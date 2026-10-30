package chstore

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func s2SelectionRows(t *testing.T, s *Store, selection readSelection, columns string) []string {
	t.Helper()
	rows, err := s.db.Conn().Query(context.Background(), "SELECT toJSONString(tuple("+columns+")) AS row FROM ("+selection.SQL+") ORDER BY row", selection.Args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	result := []string{}
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestS2MixedReadParity(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	events := syntheticDifferentialEvents(t)
	start := events[0].RequestedAt.Truncate(time.Hour)
	cutoff := start.Add(72 * time.Hour)
	if err := s.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	// Two outcome sequences must retain the first patch. Embedded responses keep priority.
	for _, status := range []int{201, 502} {
		patches := []model.ProxyResponsePatch{{ProxyRequestID: events[80].ProxyRequestID, StatusCode: status, RespondedAt: events[80].RequestedAt.Add(time.Second)}}
		if err := s.WriteBatchWithPatches(ctx, nil, patches); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.Conn().Exec(ctx, "INSERT INTO "+s.table("analytics_events")+" SELECT * REPLACE (? AS version,toUInt8(1) AS is_deleted) FROM "+s.table("analytics_events")+" FINAL WHERE attempt_id=?", s.nextVersion(), events[90].AttemptID); err != nil {
		t.Fatal(err)
	}
	var retention RetentionResult
	if err := s.c4bPromote(ctx, cutoff, false, &retention); err != nil {
		t.Fatal(err)
	}
	if err := s.c4bPromote(ctx, start.Add(48*time.Hour), true, &retention); err != nil {
		t.Fatal(err)
	}
	if err := s.c4bCleanup(ctx, s.retentionCutoff.UnixNano(), false); err != nil {
		t.Fatal(err)
	}
	for _, zone := range []string{"UTC", "America/New_York"} {
		for _, bounds := range [][2]time.Time{{start, start.Add(120 * time.Hour)}, {cutoff, cutoff.Add(24 * time.Hour)}, {start.Add(time.Minute), cutoff}, {start, cutoff.Add(-time.Minute)}, {cutoff.Add(-time.Hour), cutoff}, {start.Add(-24 * time.Hour), start}} {
			for _, filtered := range []bool{false, true} {
				q := model.Query{Operation: model.OperationSummary, Start: bounds[0], End: bounds[1], TimeZone: zone}
				if filtered {
					q.Filters = map[string]json.RawMessage{"provider": json.RawMessage(`["provider-a"]`)}
				}
				before := s.s2LegacyValidateRetainedRange(ctx, q)
				after := s.validateRetainedRange(ctx, q)
				if fmt.Sprintf("%T:%v", before, before) != fmt.Sprintf("%T:%v", after, after) {
					t.Fatalf("range %+v: before=%v after=%v", q, before, after)
				}
				for _, requests := range []bool{false, true} {
					columns := []string{"bucket_start_ns", "bucket_end_ns", "proxy_requests", "upstream_attempts"}
					if requests {
						columns = []string{"bucket_start_ns", "bucket_end_ns", "proxy_request_id"}
					}
					a, err := s.s2LegacyRetainedSelection(ctx, q, requests, columns...)
					if err != nil {
						t.Fatal(err)
					}
					b, err := s.retainedSelection(ctx, q, requests, columns...)
					if err != nil {
						t.Fatal(err)
					}
					projection := "bucket_start_ns,bucket_end_ns,proxy_requests,upstream_attempts"
					if requests {
						projection = "bucket_start_ns,bucket_end_ns,proxy_request_id"
					}
					if !reflect.DeepEqual(s2SelectionRows(t, s, a, projection), s2SelectionRows(t, s, b, projection)) {
						t.Fatalf("retained parity requests=%t query=%+v", requests, q)
					}
				}
				columns := []string{"attempt_id", "proxy_status_code", "proxy_error", "responded_at_ns"}
				a, err := s.s2LegacyRawSelection(ctx, q, columns...)
				if err != nil {
					t.Fatal(err)
				}
				b, err := s.rawSelection(ctx, q, columns...)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(s2SelectionRows(t, s, a, "attempt_id,proxy_status_code,proxy_error,responded_at_ns"), s2SelectionRows(t, s, b, "attempt_id,proxy_status_code,proxy_error,responded_at_ns")) {
					t.Fatalf("raw parity query=%+v", q)
				}
			}
		}
	}
	for _, zone := range []string{"UTC", "America/New_York"} {
		for _, bounds := range [][2]time.Time{{start, start.Add(120 * time.Hour)}, {start.Add(time.Minute), cutoff}, {cutoff, cutoff.Add(time.Hour)}} {
			q := model.Query{Operation: model.OperationActivity, Window: "year", Start: bounds[0], End: bounds[1], TimeZone: zone}
			before, after := s.s2LegacyValidateYearActivity(ctx, q), s.validateYearActivity(ctx, q)
			if fmt.Sprintf("%T:%v", before, before) != fmt.Sprintf("%T:%v", after, after) {
				t.Fatalf("year activity %+v before=%v after=%v", q, before, after)
			}
		}
	}
	for _, width := range []string{"5m", "1h", "1d"} {
		q := model.Query{Operation: model.OperationTimeseries, Start: start, End: cutoff, TimeZone: "UTC", BucketWidth: width}
		before, after := s.s2LegacyValidateRetainedRange(ctx, q), s.validateRetainedRange(ctx, q)
		if fmt.Sprintf("%T:%v", before, before) != fmt.Sprintf("%T:%v", after, after) {
			t.Fatalf("width %s: before=%v after=%v", width, before, after)
		}
	}
	q := model.Query{SchemaVersion: 2, Operation: model.OperationModelLeaderboard, Start: cutoff, End: start.Add(120 * time.Hour), TimeZone: "UTC"}
	a, err := s.s2LegacyModelLeaderboard(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.ModelLeaderboard(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("awards before=%+v after=%+v", a, b)
	}
}

func TestS2BatchCohortsMatchIndependentCausalReads(t *testing.T) {
	s, target, _ := storeEstimatorFixture(t)
	ctx := context.Background()
	targets := []model.Event{target, target, target}
	targets[1].RequestedAt = target.RequestedAt.Add(-40 * time.Minute)
	targets[2].KeyID = model.KeyID("other-key")
	histories, err := s.arrivalEstimateHistories(ctx, targets)
	if err != nil {
		t.Fatal(err)
	}
	for i, target := range targets {
		old, err := s.s2LegacyArrivalEstimateHistory(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(model.EstimateArrivalThroughput(target, old), model.EstimateArrivalThroughput(target, histories[i])) {
			t.Fatalf("cohort %d differs", i)
		}
	}
}

func TestS2CutoffSnapshotDoesNotCrossOperations(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.setMetadataBatch(ctx, map[string]string{"retention_raw_cutoff_ns": "100", "retention_raw_checkpoint_ns": "101", "retention_hourly_cutoff_ns": "50"}); err != nil {
		t.Fatal(err)
	}
	operation := withReadCutoffs(ctx)
	a, err := s.readCutoffs(operation)
	if err != nil {
		t.Fatal(err)
	}
	if a.raw != 102 || a.hourly != 50 {
		t.Fatalf("bounds=%+v", a)
	}
	if err := s.setMetadata(ctx, "retention_raw_cutoff_ns", "200"); err != nil {
		t.Fatal(err)
	}
	b, err := s.readCutoffs(operation)
	if err != nil || a != b {
		t.Fatalf("snapshot changed %+v %v", b, err)
	}
	c, err := s.readCutoffs(withReadCutoffs(ctx))
	if err != nil || c.raw != 200 {
		t.Fatalf("next operation=%+v %v", c, err)
	}
}

func TestS2BatchCohortsMaxPage(t *testing.T) {
	s, target, _ := storeEstimatorFixture(t)
	targets := make([]model.Event, model.MaxPageSize)
	for i := range targets {
		targets[i] = target
	}
	histories, err := s.arrivalEstimateHistories(context.Background(), targets)
	if err != nil {
		t.Fatal(err)
	}
	for i, history := range histories {
		if len(history) != 70 {
			t.Fatalf("target %d labels=%d want=70", i, len(history))
		}
	}
}
