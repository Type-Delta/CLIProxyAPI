package chstore

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

type c4bTotals struct{ Attempts, Tokens, Money, Requests int64 }

// This is the schema's reader publication rule, evaluated independently of C2.
// The UNION's exact identity count catches both cross-grain and cross-dimension retries.
func c4bVisible(t *testing.T, s *Store) c4bTotals {
	t.Helper()
	ctx := context.Background()
	raw, err := s.c4bBoundary(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	hourly, err := s.c4bBoundary(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	visible := `is_deleted=0 AND ((grain='hourly' AND bucket_end_ns<=? AND bucket_start_ns>=?) OR (grain='daily' AND bucket_end_ns<=?)) AND ` + s.c4bPublishedPredicate()
	var out c4bTotals
	query := "SELECT sum(attempts),sum(tokens),sum(money) FROM (SELECT toInt64(count()) attempts,sum(total_tokens) tokens,sum(ifNull(known_cost_nano,0)) money FROM " + s.table("analytics_events") + " FINAL WHERE is_deleted=0 AND requested_at_ns>=? UNION ALL SELECT sum(upstream_attempts),sum(total_tokens),sum(known_cost_nano) FROM " + s.table("analytics_rollups") + " FINAL WHERE " + visible + ")"
	if err = s.db.Conn().QueryRow(ctx, query, raw, raw, hourly, hourly).Scan(&out.Attempts, &out.Tokens, &out.Money); err != nil {
		t.Fatal(err)
	}
	query = "SELECT toInt64(uniqExact(proxy_request_id)) FROM (SELECT proxy_request_id FROM " + s.table("analytics_events") + " FINAL WHERE is_deleted=0 AND requested_at_ns>=? UNION ALL SELECT proxy_request_id FROM " + s.table("analytics_request_rollups") + " FINAL WHERE " + visible + ")"
	if err = s.db.Conn().QueryRow(ctx, query, raw, raw, hourly, hourly).Scan(&out.Requests); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestC4bRetentionCrashPublication(t *testing.T) {
	t.Parallel()
	for _, grain := range []string{"raw", "hourly"} {
		steps := []string{"begin", "totals", "requests", "published", "deleted"}
		if grain == "raw" {
			steps = append(steps, "outcomes-deleted")
		}
		if grain == "hourly" {
			steps = append(steps, "source-analytics_rollups-deleted", "source-analytics_request_rollups-deleted")
		}
		for _, step := range steps {
			t.Run(grain+"/"+step, func(t *testing.T) {
				s := openTestStore(t)
				events := loadFixtureEvents(t)[:3]
				for i := range events {
					events[i].RequestedAt = time.Date(2026, 8, 12, 4, i, 0, 0, time.UTC)
				}
				events[1].ProxyRequestID = events[0].ProxyRequestID
				if err := s.WriteBatch(context.Background(), events); err != nil {
					t.Fatal(err)
				}
				want := c4bVisible(t, s)
				rawCut := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
				if grain == "hourly" {
					if _, err := s.ApplyRetention(context.Background(), rawCut, 1); err != nil {
						t.Fatal(err)
					}
				}
				injected := errors.New("simulated process interruption")
				ctx := context.WithValue(context.Background(), c4bStepKey{}, func(name string) error {
					if name == grain+":"+step {
						return injected
					}
					return nil
				})
				dailyCut := time.Time{}
				if grain == "hourly" {
					dailyCut = rawCut
				}
				if _, err := s.ApplyRetentionPolicy(ctx, rawCut, dailyCut, 1); !errors.Is(err, injected) {
					t.Fatalf("error=%v", err)
				}
				if got := c4bVisible(t, s); got != want {
					t.Fatalf("interrupted totals=%+v want=%+v", got, want)
				}
				for retry := 0; retry < 2; retry++ {
					if _, err := s.ApplyRetentionPolicy(context.Background(), rawCut, dailyCut, 1); err != nil {
						t.Fatal(err)
					}
					if got := c4bVisible(t, s); got != want {
						t.Fatalf("retry totals=%+v want=%+v", got, want)
					}
				}
			})
		}
	}
}

func TestC4bRetentionBucketsAndOwnership(t *testing.T) {
	t.Parallel()
	for _, zone := range []string{"UTC", "Asia/Kolkata", "America/St_Johns"} {
		t.Run(zone, func(t *testing.T) {
			location, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Date(2026, 11, 1, 0, 0, 0, 0, location)
			end := start.AddDate(0, 0, 1).UTC()
			config := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, RetentionTimeZone: zone, PriceBook: fixturePriceBook()}
			s := openTestStore(t, config)
			events := loadFixtureEvents(t)[:3]
			for i := range events {
				events[i].RequestedAt = start.Add(time.Duration(i*60+5) * time.Minute).UTC()
			}
			events[1].ProxyRequestID = events[0].ProxyRequestID
			if _, err = s.WriteImportBatch(context.Background(), events, "retained-batch"); err != nil {
				t.Fatal(err)
			}
			want := c4bVisible(t, s)
			if _, err = s.ApplyRetentionPolicy(context.Background(), end, time.Time{}, 1); err != nil {
				t.Fatal(err)
			}
			if got := c4bVisible(t, s); got != want {
				t.Fatalf("hourly=%+v want=%+v", got, want)
			}
			if _, err = s.ApplyRetentionPolicy(context.Background(), end, end, 1); err != nil {
				t.Fatal(err)
			}
			if got := c4bVisible(t, s); got != want {
				t.Fatalf("daily=%+v want=%+v", got, want)
			}
			var minimum, maximum, owned int64
			if err = s.db.Conn().QueryRow(context.Background(), "SELECT min(bucket_start_ns),max(bucket_end_ns),sum(upstream_attempts) FROM "+s.table("analytics_rollups")+" FINAL WHERE grain='daily' AND is_deleted=0 AND import_batch_id='retained-batch'").Scan(&minimum, &maximum, &owned); err != nil {
				t.Fatal(err)
			}
			if minimum != start.UTC().UnixNano() || maximum != end.UnixNano() || owned != int64(len(events)) {
				t.Fatalf("daily bounds=%v..%v owned=%d", time.Unix(0, minimum), time.Unix(0, maximum), owned)
			}
		})
	}
}

func TestC4bRetentionValidationAndPartialBucket(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	events := loadFixtureEvents(t)[:1]
	events[0].RequestedAt = time.Date(2026, 8, 12, 4, 5, 0, 0, time.UTC)
	if err := s.WriteBatch(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	want := c4bVisible(t, s)
	cutoff := events[0].RequestedAt.Add(10 * time.Minute)
	result, err := s.ApplyRetention(context.Background(), cutoff, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedRows != 0 {
		t.Fatal(result)
	}
	if got := c4bVisible(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("partial hour totals=%+v want=%+v", got, want)
	}
	for _, size := range []int{0, 10001} {
		if _, err = s.ApplyRetention(context.Background(), cutoff, size); err == nil {
			t.Fatalf("accepted batch size %d", size)
		}
	}
	if _, err = s.ApplyRetentionPolicy(context.Background(), cutoff, cutoff.Add(time.Hour), 1); err == nil {
		t.Fatal("accepted inverted policy")
	}
	if _, err = s.ApplyRetention(context.Background(), cutoff.In(time.FixedZone("offset", 3600)), 1); err == nil {
		t.Fatal("accepted non-UTC cutoff")
	}
}

func TestC4bRetentionRemovesOnlyRetiredOutcomeReferences(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	events := loadFixtureEvents(t)[:2]
	events[0].RequestedAt = time.Date(2026, 8, 12, 4, 5, 0, 0, time.UTC)
	events[1].RequestedAt = events[0].RequestedAt.Add(2 * time.Hour)
	events[1].ProxyRequestID = events[0].ProxyRequestID
	if err := s.WriteBatchWithPatches(ctx, events, []model.ProxyResponsePatch{{ProxyRequestID: events[0].ProxyRequestID, StatusCode: 502, Error: "old patch", RespondedAt: events[1].RequestedAt.Add(time.Second)}}); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 8, 12, 5, 0, 0, 0, time.UTC)
	if _, err := s.ApplyRetention(ctx, cutoff, 1); err != nil {
		t.Fatal(err)
	}
	replay := events[0]
	replay.RequestedAt = cutoff.Add(3 * time.Hour)
	if err := s.WriteBatch(ctx, []model.Event{replay}); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.loadEvents(ctx, "attempt_id IN (?)", []string{events[0].AttemptID, events[1].AttemptID})
	if err != nil || len(loaded) != 2 {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	for _, event := range loaded {
		if event.AttemptID == replay.AttemptID && (event.ProxyStatusCode != nil || event.RespondedAt != nil || event.ProxyError != nil) {
			t.Fatalf("reinsert inherited retained patch: %+v", event)
		}
		if event.AttemptID == events[1].AttemptID && (event.ProxyStatusCode == nil || *event.ProxyStatusCode != 502) {
			t.Fatalf("surviving attempt lost patch: %+v", event)
		}
	}
}

func TestC4bRetainedCacheDimensionsAndPartialRanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	events := loadFixtureEvents(t)[:3]
	for i := range events {
		events[i].RequestedAt = time.Date(2026, 8, 12, 4+i, 15, 0, 0, time.UTC)
		events[i].Tokens.Cached = 40
		events[i].Tokens.CacheRead = 0
	}
	events[2].Tokens.Cached = 0
	events[2].Tokens.CacheRead = 25
	if err := s.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	q := model.Query{SchemaVersion: 1, Operation: model.OperationDimensions, Dimension: "cache", Start: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC), TimeZone: "UTC", PageSize: 100}
	want, err := s.Dimensions(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(want.Rows) != 2 {
		t.Fatalf("cache groups=%+v", want.Rows)
	}
	for _, daily := range []time.Time{{}, q.End} {
		if _, err = s.ApplyRetentionPolicy(ctx, q.End, daily, 1); err != nil {
			t.Fatal(err)
		}
		got, err := s.Dimensions(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Rows, want.Rows) {
			t.Fatalf("retained cache=%+v want=%+v", got.Rows, want.Rows)
		}
		partial := q
		partial.Start = events[0].RequestedAt
		partial.Operation = model.OperationSummary
		partial.Dimension = ""
		partial.PageSize = 0
		if _, err = s.Summary(ctx, partial); !errors.Is(err, ErrRetainedRangePartial) {
			t.Fatalf("partial range error=%v", err)
		}
	}
}
