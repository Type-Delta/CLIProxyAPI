package chstore

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	sqlstore "github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

func r1ComparePages(t *testing.T, f differentialFixture, q model.Query) {
	t.Helper()
	reader := diffReader(model.OperationDimensions, (*Store).Dimensions, (*sqlstore.SQLiteStore).Dimensions)
	if q.Operation == model.OperationLeaderboard {
		reader = diffReader(model.OperationLeaderboard, (*Store).Leaderboard, (*sqlstore.SQLiteStore).Leaderboard)
	}
	cq, sq := q, q
	for page := 0; page < 200; page++ {
		got, e1 := reader.ch(context.Background(), f.ch, cq)
		want, e2 := reader.sqlite(context.Background(), f.sqlite, sq)
		if e1 != nil || e2 != nil {
			if e1 == nil || e2 == nil || e1.Error() != e2.Error() {
				t.Fatalf("errors differ: %v / %v", e1, e2)
			}
			return
		}
		got, cq.Cursor = normalizeDiffPage(got)
		want, sq.Cursor = normalizeDiffPage(want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("page %d differs\nCH %+v\nSQLite %+v", page, got, want)
		}
		if (cq.Cursor == "") != (sq.Cursor == "") {
			t.Fatal("cursor presence differs")
		}
		if cq.Cursor == "" {
			return
		}
		a, err := f.codec.Decode(cq.Cursor)
		if err != nil {
			t.Fatal(err)
		}
		b, err := f.codec.Decode(sq.Cursor)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("cursors differ: %+v / %+v", a, b)
		}
	}
	t.Fatal("pagination did not finish")
}

// Covers the raw/compact/retained SQLite dimension paths with the same oracle.
// CH keeps one SQL aggregation path for every range and storage grain.
func TestC2R1DimensionsLeaderboardMatrix(t *testing.T) {
	t.Parallel()
	events := syntheticDifferentialEvents(t)
	for _, zone := range []string{"UTC", "America/New_York", "Asia/Kathmandu"} {
		t.Run(zone, func(t *testing.T) {
			t.Parallel()
			f := openDifferentialFixture(t, events, 0, zone)
			start, _, err := aggregate.BucketBounds(events[0].RequestedAt, zone, "1d")
			if err != nil {
				t.Fatal(err)
			}
			_, end, err := aggregate.BucketBounds(events[len(events)-1].RequestedAt, zone, "1d")
			if err != nil {
				t.Fatal(err)
			}
			loc, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			raw := start.In(loc).AddDate(0, 0, 3).UTC()
			base := model.Query{SchemaVersion: 2, Start: start, End: end, TimeZone: zone, PageSize: 1}
			for _, phase := range []string{"raw", "hourly", "daily"} {
				t.Run(phase, func(t *testing.T) {
					if phase != "raw" {
						hourly := time.Time{}
						if phase == "daily" {
							hourly = start.In(loc).AddDate(0, 0, 2).UTC()
						}
						if _, err := f.sqlite.ApplyRetentionPolicy(context.Background(), raw, hourly, 1000); err != nil {
							t.Fatal(err)
						}
						f.copyRetained(t, raw, hourly)
					}
					for _, dimension := range []string{"provider", "model", "credential", "key", "endpoint", "failure", "latency", "cache", "service_tier", "source"} {
						t.Run(dimension, func(t *testing.T) {
							q := base
							q.Operation = model.OperationDimensions
							q.Dimension = dimension
							r1ComparePages(t, f, q)
						})
					}
					for _, sort := range []model.LeaderboardSort{model.LeaderboardSortTokens, model.LeaderboardSortCost} {
						for _, size := range []int{1, 100} {
							t.Run(fmt.Sprintf("%s/%d", sort, size), func(t *testing.T) {
								q := base
								q.Operation = model.OperationLeaderboard
								q.SortBy = sort
								q.PageSize = size
								r1ComparePages(t, f, q)
							})
						}
					}
				})
			}
		})
	}
}

// Same cache and source expectations as query_v2_test.go, including the legacy
// cached count being different from normalized cache-read tokens.
func TestC2R1CacheAndSourceDimensions(t *testing.T) {
	t.Parallel()
	events := loadFixtureEvents(t)[:3]
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := range events {
		events[i].RequestedAt = start.Add(time.Duration(i*5+1) * time.Minute)
		events[i].Tokens.Cached = 0
		events[i].Tokens.CacheRead = 0
	}
	events[0].Tokens.Cached = 99
	events[1].Tokens.CacheRead = 20
	events[2].Tokens.CacheRead = 40
	f := openDifferentialFixture(t, events, 0)
	q := model.Query{SchemaVersion: 2, Operation: model.OperationDimensions, Dimension: "cache", Start: start, End: start.Add(24 * time.Hour), TimeZone: "UTC", PageSize: 10}
	for _, phase := range []string{"raw", "hourly", "daily"} {
		t.Run(phase, func(t *testing.T) {
			if phase != "raw" {
				daily := time.Time{}
				if phase == "daily" {
					daily = start.Add(24 * time.Hour)
				}
				if _, err := f.sqlite.ApplyRetentionPolicy(context.Background(), start.Add(24*time.Hour), daily, 100); err != nil {
					t.Fatal(err)
				}
				f.copyRetained(t, start.Add(24*time.Hour), daily)
			}
			q.Dimension = "cache"
			page, err := f.ch.Dimensions(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int64{}
			for _, r := range page.Rows {
				counts[r.Value] = r.UpstreamAttempts
			}
			if counts["cached"] != 2 || counts["uncached"] != 1 {
				t.Fatalf("cache rows: %+v", page.Rows)
			}
			r1ComparePages(t, f, q)
			q.Dimension = "source"
			page, err = f.ch.Dimensions(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			counts = map[string]int64{}
			for _, r := range page.Rows {
				counts[r.Value] = r.UpstreamAttempts
			}
			if counts["native"] != 2 || counts["import"] != 1 {
				t.Fatalf("source rows: %+v", page.Rows)
			}
			r1ComparePages(t, f, q)
		})
	}
}

func TestC2R1LeaderboardCollisionsTiesAndCursors(t *testing.T) {
	t.Parallel()
	seed := loadFixtureEvents(t)[0]
	start := seed.RequestedAt.Truncate(time.Hour)
	var events []model.Event
	for i := 0; i < 5; i++ {
		e := seed
		e.AttemptID = fmt.Sprintf("%032x", i+100)
		e.ProxyRequestID = fmt.Sprintf("%032x", i+200)
		e.KeyID = strings.Repeat("a", 12) + fmt.Sprintf("%052x", i+1)
		e.RequestedAt = start.Add(time.Duration(i+1) * time.Minute)
		e.Tokens.Total = 200
		e.Tokens.Quality = model.TokenQualityExact
		events = append(events, e)
	}
	f := openDifferentialFixture(t, events, 0)
	q := model.Query{SchemaVersion: 2, Operation: model.OperationLeaderboard, Start: start, End: start.Add(time.Hour), TimeZone: "UTC", PageSize: 1, SortBy: model.LeaderboardSortTokens}
	for _, sort := range []model.LeaderboardSort{model.LeaderboardSortTokens, model.LeaderboardSortCost} {
		q.SortBy = sort
		r1ComparePages(t, f, q)
	}
	q.SortBy = model.LeaderboardSortTokens
	first, err := f.ch.Leaderboard(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Rows) != 1 || len(first.Rows[0].ShortKeyID) != 64 || first.Rows[0].Rank != 1 || first.Rows[0].PercentOfTotal != "20" {
		t.Fatalf("global collision/rank/percent: %+v", first)
	}
	cursor, err := f.codec.Decode(first.Meta.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	cursor.Metric = "201"
	q.Cursor, err = f.codec.Encode(cursor)
	if err != nil {
		t.Fatal(err)
	}
	r1ComparePages(t, f, q)
	if _, err := f.ch.Leaderboard(context.Background(), q); err == nil || err.Error() != "leaderboard cursor is stale" {
		t.Fatalf("stale cursor: %v", err)
	}
	q.Cursor = first.Meta.NextCursor
	q.Filters = map[string]json.RawMessage{"success": json.RawMessage("false")}
	r1ComparePages(t, f, q)
}

func TestC2R1DistinctRequestsAcrossRetentionBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	seed := loadFixtureEvents(t)[0]
	start := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	var events []model.Event
	for i := 0; i < 4; i++ {
		e := seed
		e.AttemptID = fmt.Sprintf("%032x", i+800)
		e.RequestedAt = start.Add(time.Duration(i+1) * time.Hour)
		e.Provider = "provider-retained"
		e.KeyID = model.KeyID("r1-key")
		e.ProxyRequestID = strings.Repeat("b", 32)
		if i == 3 {
			e.ProxyRequestID = strings.Repeat("c", 32)
		}
		e.Tokens.Total = 100
		events = append(events, e)
	}
	f := openDifferentialFixture(t, events, 0)
	raw := start.Add(3 * time.Hour)
	if _, err := f.sqlite.ApplyRetentionPolicy(ctx, raw, time.Time{}, 100); err != nil {
		t.Fatal(err)
	}
	f.copyRetained(t, raw, time.Time{})
	q := model.Query{SchemaVersion: 2, Operation: model.OperationDimensions, Dimension: "key", Start: start, End: start.Add(24 * time.Hour), TimeZone: "UTC", PageSize: 10}
	page, err := f.ch.Dimensions(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 || page.Rows[0].ProxyRequests != 2 || page.Rows[0].UpstreamAttempts != 4 || page.Rows[0].Tokens.Total != 400 {
		t.Fatalf("distinct requests across history: %+v", page.Rows)
	}
	r1ComparePages(t, f, q)
	q.Operation = model.OperationLeaderboard
	q.SortBy = model.LeaderboardSortTokens
	r1ComparePages(t, f, q)
	// Partial retained buckets outside the selected key/provider must not reject
	// a raw-only selection. Matching partial history must still fail.
	q.Start = start.Add(90 * time.Minute)
	q.Filters = map[string]json.RawMessage{"provider": json.RawMessage(`["absent"]`)}
	r1ComparePages(t, f, q)
	q.Filters = map[string]json.RawMessage{"provider": json.RawMessage(`["provider-retained"]`)}
	r1ComparePages(t, f, q)
}
