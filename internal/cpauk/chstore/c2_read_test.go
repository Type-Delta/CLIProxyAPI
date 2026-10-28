package chstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestC2RawSelectionProjectionAndOutcomes(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	events := loadFixtureEvents(t)
	first := events[0]
	first.ProxyStatusCode = nil
	first.ProxyError = nil
	first.RespondedAt = nil
	if err := s.WriteBatchWithPatches(ctx, []model.Event{first}, []model.ProxyResponsePatch{{ProxyRequestID: first.ProxyRequestID, StatusCode: 502, Error: "first", RespondedAt: first.RequestedAt.Add(time.Second)}}); err != nil {
		t.Fatal(err)
	}
	later := first
	later.AttemptID = fmt.Sprintf("%032x", 8001)
	recorded := first
	recorded.AttemptID = fmt.Sprintf("%032x", 8002)
	status := 201
	responded := first.RequestedAt.Add(4 * time.Second)
	recorded.ProxyStatusCode = &status
	recorded.RespondedAt = &responded
	if err := s.WriteBatchWithPatches(ctx, []model.Event{later, recorded}, []model.ProxyResponsePatch{{ProxyRequestID: first.ProxyRequestID, StatusCode: 200, RespondedAt: first.RequestedAt.Add(2 * time.Second)}}); err != nil {
		t.Fatal(err)
	}
	q := model.Query{SchemaVersion: 2, Operation: model.OperationEvents, Start: first.RequestedAt.Add(-time.Hour), End: first.RequestedAt.Add(time.Hour), TimeZone: "UTC", PageSize: 50}
	selection, err := s.rawSelection(ctx, q, "attempt_id", "proxy_status_code", "proxy_error", "responded_at_ns", "session_captured")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(selection.SQL, "event_json") {
		t.Fatal("scalar selection read event JSON")
	}
	rows, err := s.db.Conn().Query(ctx, selection.SQL, selection.Args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		var id string
		var status, at *int64
		var message *string
		var captured bool
		if err := rows.Scan(&id, &status, &message, &at, &captured); err != nil {
			t.Fatal(err)
		}
		if !captured {
			t.Fatal("absent session boundary did not capture native row")
		}
		want := int64(200)
		if id == first.AttemptID {
			want = 502
			if message == nil || *message != "first" {
				t.Fatal("lost first error")
			}
		} else if id == recorded.AttemptID {
			want = 201
		} else if message != nil {
			t.Fatal("empty patch error should be null")
		}
		if status == nil || *status != want || at == nil {
			t.Fatalf("overlay %s status=%v want=%d", id, status, want)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("rows=%d", count)
	}
	_ = rows.Close()
	q.Filters = map[string]json.RawMessage{"provider": json.RawMessage(`["x') OR 1=1 --"]`)}
	selected, err := s.rawSelection(ctx, q, "attempt_id")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(selected.SQL, "OR 1=1 --") {
		t.Fatal("filter interpolated into SQL")
	}
	var n uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM ("+selected.SQL+")", selected.Args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("parameter filter ignored")
	}
}

func TestC2RawSelectionFinalBeforeFilters(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	e := loadFixtureEvents(t)[0]
	if err := s.WriteBatch(ctx, []model.Event{e}); err != nil {
		t.Fatal(err)
	}
	e.Provider = "replacement"
	if _, err := s.insertEvents(ctx, []model.Event{e}, "", true); err != nil {
		t.Fatal(err)
	}
	q := model.Query{Operation: model.OperationSummary, Start: e.RequestedAt.Add(-time.Hour), End: e.RequestedAt.Add(time.Hour), TimeZone: "UTC"}
	for _, provider := range []string{"replacement", loadFixtureEvents(t)[0].Provider} {
		q.Filters = map[string]json.RawMessage{"provider": json.RawMessage(fmt.Sprintf("[%q]", provider))}
		selected, err := s.rawSelection(ctx, q, "attempt_id")
		if err != nil {
			t.Fatal(err)
		}
		var n uint64
		if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM ("+selected.SQL+")", selected.Args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		want := uint64(0)
		if provider == "replacement" {
			want = 1
		}
		if n != want {
			t.Fatalf("provider %s rows=%d want=%d", provider, n, want)
		}
	}
	if err := s.db.Conn().Exec(ctx, "INSERT INTO "+s.table("analytics_events")+" SELECT * REPLACE (? AS version,toUInt8(1) AS is_deleted) FROM "+s.table("analytics_events")+" FINAL WHERE attempt_id=?", s.nextVersion(), e.AttemptID); err != nil {
		t.Fatal(err)
	}
	q.Filters = nil
	selected, err := s.rawSelection(ctx, q, "attempt_id")
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM ("+selected.SQL+")", selected.Args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("tombstone resurrected an old row")
	}
}

func TestC2BucketSQLMatchesGo(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	instants := []time.Time{time.Date(1969, 12, 31, 23, 59, 59, 123, time.UTC), time.Date(2018, 11, 4, 12, 0, 0, 0, time.UTC), time.Date(2026, 3, 8, 6, 59, 59, 0, time.UTC), time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC), time.Date(2026, 10, 3, 15, 45, 0, 0, time.UTC)}
	for _, zone := range []string{"UTC", "America/New_York", "Asia/Kathmandu", "Australia/Lord_Howe", "America/Sao_Paulo"} {
		for _, width := range []string{"1m", "5m", "15m", "30m", "1h", "1d", "1w"} {
			timestamps := make([]int64, len(instants))
			for i, at := range instants {
				timestamps[i] = at.UnixNano()
			}
			q := model.Query{TimeZone: zone, BucketWidth: width}
			rows, err := s.db.Conn().Query(ctx, "SELECT at_ns,"+coreBucketExpression("at_ns", q)+" FROM (SELECT arrayJoin(?) AS at_ns)", timestamps)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var atNS, got int64
				if err = rows.Scan(&atNS, &got); err != nil {
					_ = rows.Close()
					t.Fatal(err)
				}
				at := time.Unix(0, atNS).UTC()
				start, _, errBounds := aggregate.BucketBounds(at, zone, width)
				if errBounds != nil {
					_ = rows.Close()
					t.Fatal(errBounds)
				}
				if got != start.UnixNano() {
					_ = rows.Close()
					t.Fatalf("zone=%s width=%s at=%s got=%s want=%s", zone, width, at, time.Unix(0, got).UTC(), start)
				}
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestC2UnpublishedRetainedRowsAreInvisible(t *testing.T) {
	t.Parallel()
	events := syntheticDifferentialEvents(t)
	f := openDifferentialFixture(t, events, events[0].RequestedAt.UnixNano())
	ctx := context.Background()
	raw := events[0].RequestedAt.Add(3 * 24 * time.Hour)
	if _, err := f.sqlite.ApplyRetentionPolicy(ctx, raw, time.Time{}, 1000); err != nil {
		t.Fatal(err)
	}
	f.copyRetained(t, raw, time.Time{})
	// Physical raw rows remain. Staged daily and future hourly destinations must
	// not become visible until metadata publishes their source cutoffs.
	for _, table := range []string{"analytics_rollups", "analytics_request_rollups"} {
		if err := f.ch.db.Conn().Exec(ctx, "INSERT INTO "+f.ch.table(table)+" SELECT * REPLACE ('daily' AS grain,? AS version) FROM "+f.ch.table(table)+" FINAL WHERE grain='hourly'", f.ch.nextVersion()); err != nil {
			t.Fatal(err)
		}
		if err := f.ch.db.Conn().Exec(ctx, "INSERT INTO "+f.ch.table(table)+" SELECT * REPLACE (? AS bucket_start_ns,? AS bucket_end_ns,? AS version) FROM "+f.ch.table(table)+" FINAL WHERE grain='hourly' LIMIT 1", raw.UnixNano(), raw.Add(time.Hour).UnixNano(), f.ch.nextVersion()); err != nil {
			t.Fatal(err)
		}
	}
	q := model.Query{SchemaVersion: 2, Start: events[0].RequestedAt, End: events[len(events)-1].RequestedAt.Add(time.Hour), TimeZone: "UTC"}
	for _, reader := range c2DifferentialReaders[:2] {
		f.compare(t, reader, q)
	}
}
