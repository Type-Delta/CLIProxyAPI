package chstore

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func s5AnalysisCostParity(t *testing.T, s *Store, ctx context.Context, q model.Query) {
	t.Helper()
	old, oe := s.s5LegacyAnalysisCosts(ctx, q)
	got, ge := s.analysisCosts(ctx, q)
	if fmt.Sprint(oe) != fmt.Sprint(ge) || !reflect.DeepEqual(old, got) {
		t.Errorf("parity mismatch\nold=%+v err=%v\nnew=%+v err=%v", old, oe, got, ge)
	}
}

// Sessions with join_use_nulls=1 must price like the default join.
func TestS5AnalysisCostsJoinUseNulls(t *testing.T) {
	s := openTestStore(t)
	es := loadFixtureEvents(t)
	if err := s.WriteBatch(context.Background(), es[:1]); err != nil {
		t.Fatal(err)
	}
	initialQ := c2r2V2Query(model.OperationAnalysis, es[0].RequestedAt.Add(-24*time.Hour), es[0].RequestedAt.Add(24*time.Hour))
	nullCtx := ch.Context(context.Background(), ch.WithSettings(ch.Settings{"join_use_nulls": 1}))
	s5AnalysisCostParity(t, s, nullCtx, initialQ)
	es[0].AttemptID = strings.Repeat("f", 32)
	es[0].Model = "unpriced-unknown"
	if err := s.WriteBatch(context.Background(), es[:1]); err != nil {
		t.Fatal(err)
	}
	q := c2r2V2Query(model.OperationAnalysis, es[0].RequestedAt.Add(-24*time.Hour), es[0].RequestedAt.Add(24*time.Hour))
	ctx := ch.Context(context.Background(), ch.WithSettings(ch.Settings{"join_use_nulls": 1}))
	s5AnalysisCostParity(t, s, ctx, q)
}

// The aligned pricing window starts at the storage-calendar day even when the
// zone skips local midnight.
func TestS5PricingMissingSkippedMidnight(t *testing.T) {
	ctx := context.Background()
	zone := "America/Havana"
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 3, 8, 12, 30, 0, 0, loc).UTC()
	boundary, _, err := aggregate.BucketBounds(start, zone, "1d")
	if err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), RetentionTimeZone: zone, MaxStorageBytes: 64 << 20})
	var es []model.Event
	for i, at := range []time.Time{boundary.Add(-30 * time.Minute), boundary.Add(30 * time.Minute)} {
		id := fmt.Sprintf("%032x", i+1)
		es = append(es, c2r2V2Event(id, id, model.KeyID("s5"), at, true, nil, 0, 0, 1, 2))
	}
	if err := s.WriteBatch(ctx, es); err != nil {
		t.Fatal(err)
	}
	for _, daily := range []bool{false, true} {
		hourly := time.Time{}
		if daily {
			hourly = start.Add(72 * time.Hour).UTC()
		}
		if _, err := s.ApplyRetentionPolicy(ctx, start.Add(96*time.Hour).UTC(), hourly, 1000); err != nil {
			t.Fatal(err)
		}
		got, err := s.PricingMissing(ctx, model.Range{Start: start, End: start.Add(model.MaxQueryRangeDays * 24 * time.Hour), TimeZone: "UTC"})
		if err != nil || len(got) != 1 || got[0].Requests != 1 {
			t.Errorf("daily=%v expected one current-calendar-day event, got %+v %v", daily, got, err)
		}
	}
}
