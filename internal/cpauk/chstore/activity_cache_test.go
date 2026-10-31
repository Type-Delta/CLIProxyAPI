package chstore

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestActivityCacheClosedDaysAndOpenDay(t *testing.T) {
	t.Parallel()
	for _, zone := range []string{"UTC", "America/St_Johns", "America/Santiago", "Australia/Lord_Howe", "Asia/Kathmandu"} {
		t.Run(zone, func(t *testing.T) {
			s := openTestStore(t)
			now := time.Date(2026, 9, 6, 16, 0, 0, 0, time.UTC)
			s.nowFunc = func() time.Time { return now }
			today, err := activityDayStart(now, zone)
			if err != nil {
				t.Fatal(err)
			}
			location, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			start := today.In(location).AddDate(0, 0, -2).UTC()
			event := loadFixtureEvents(t)[0]
			event.RequestedAt = today.Add(-time.Hour)
			if err := s.WriteBatch(context.Background(), []model.Event{event}); err != nil {
				t.Fatal(err)
			}
			q := model.Query{SchemaVersion: 2, Operation: model.OperationActivity, Start: start, End: now, TimeZone: zone, Window: "year"}
			ctx, count := historyQueryCounter()
			first, err := s.Activity(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			cold := count.Load()
			second, err := s.Activity(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			hot := count.Load() - cold
			if hot == 0 || hot >= cold {
				t.Fatalf("closed days not reused or today frozen: cold=%d warm=%d", cold, hot)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatal("warm activity differs")
			}
			historical := q
			historical.End = today
			before := count.Load()
			if _, err := s.Activity(ctx, historical); err != nil {
				t.Fatal(err)
			}
			if count.Load() != before {
				t.Fatal("closed prefix not cached")
			}
			tail := event
			tail.AttemptID = fmt.Sprintf("%032x", 200)
			tail.ProxyRequestID = tail.AttemptID
			tail.RequestedAt = today.Add(time.Hour)
			if err := s.WriteBatch(ctx, []model.Event{tail}); err != nil {
				t.Fatal(err)
			}
			before = count.Load()
			if _, err := s.Activity(ctx, historical); err != nil {
				t.Fatal(err)
			}
			if count.Load() != before {
				t.Fatal("tail invalidated closed days")
			}
			live, err := s.Activity(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			if live.Buckets[len(live.Buckets)-1].Requests != 1 {
				t.Fatal("new usage missing from today")
			}
			// Outcome-only collector batches for today's attempts must also preserve history.
			if err := s.WriteBatchWithPatches(ctx, nil, []model.ProxyResponsePatch{{ProxyRequestID: tail.ProxyRequestID, StatusCode: 200}}); err != nil {
				t.Fatal(err)
			}
			before = count.Load()
			if _, err := s.Activity(ctx, historical); err != nil {
				t.Fatal(err)
			}
			if count.Load() != before {
				t.Fatal("tail patch invalidated closed days")
			}
			late := event
			late.AttemptID = fmt.Sprintf("%032x", 201)
			late.ProxyRequestID = late.AttemptID
			if err := s.WriteBatch(ctx, []model.Event{late}); err != nil {
				t.Fatal(err)
			}
			before = count.Load()
			updated, err := s.Activity(ctx, historical)
			if err != nil {
				t.Fatal(err)
			}
			if count.Load() == before {
				t.Fatal("late event did not invalidate")
			}
			buckets, err := s.activityBuckets(ctx, historical, "1d")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(updated.Buckets, buckets) {
				t.Fatal("uncached parity")
			}
			// A query ending earlier today is still mutable and cannot be frozen.
			open := q
			open.Start = today
			open.End = now.Add(-time.Minute)
			if _, err := s.Activity(ctx, open); err != nil {
				t.Fatal(err)
			}
			before = count.Load()
			if _, err := s.Activity(ctx, open); err != nil {
				t.Fatal(err)
			}
			if count.Load() == before {
				t.Fatal("open day cached")
			}
			// Advancing the clock freezes yesterday, using a calendar boundary across DST.
			now = now.In(location).AddDate(0, 0, 1).UTC()
			closed := q
			if _, err := s.Activity(ctx, closed); err != nil {
				t.Fatal(err)
			}
			before = count.Load()
			if _, err := s.Activity(ctx, closed); err != nil {
				t.Fatal(err)
			}
			if count.Load() != before {
				t.Fatal("newly closed day not cached")
			}
		})
	}
}

func TestHistoryMutationJournalBound(t *testing.T) {
	s, q, _ := cacheFixture(t)
	key, _ := s.historyKey(q)
	for i := 0; i < historyMaxMutations+1; i++ {
		at := s.now().Add(time.Duration(i) * 15 * time.Minute)
		s.invalidateHistory(at, at.Add(time.Second))
	}
	if len(s.historyMutations) > historyMaxMutations {
		t.Fatal("unbounded journal")
	}
	next, _ := s.historyKey(q)
	if next == key {
		t.Fatal("journal overflow did not start a generation")
	}
}
