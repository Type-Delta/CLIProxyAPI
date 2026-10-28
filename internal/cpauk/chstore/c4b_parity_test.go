package chstore

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

type c4bReader interface {
	Summary(context.Context, model.Query) (model.Summary, error)
	Timeseries(context.Context, model.Query) (model.Timeseries, error)
	Dimensions(context.Context, model.Query) (model.DimensionPage, error)
	Leaderboard(context.Context, model.Query) (model.LeaderboardPage, error)
}

type c4bQuerySnapshot struct {
	Summary     model.Summary
	Series      model.Timeseries
	Dimensions  model.DimensionPage
	Leaderboard model.LeaderboardPage
}

func c4bQuery(t *testing.T, s c4bReader, base model.Query) c4bQuerySnapshot {
	t.Helper()
	ctx := context.Background()
	var out c4bQuerySnapshot
	var err error
	base.Operation = model.OperationSummary
	out.Summary, err = s.Summary(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	base.Operation = model.OperationTimeseries
	base.BucketWidth = "1d"
	out.Series, err = s.Timeseries(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	base.BucketWidth = ""
	base.Operation = model.OperationDimensions
	base.Dimension = "provider"
	base.PageSize = 100
	out.Dimensions, err = s.Dimensions(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	base.Dimension = ""
	base.Operation = model.OperationLeaderboard
	base.SortBy = model.LeaderboardSortCost
	out.Leaderboard, err = s.Leaderboard(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func c4bAssertQuery(t *testing.T, got, want c4bQuerySnapshot) {
	t.Helper()
	if got.Summary.ProxyRequests != want.Summary.ProxyRequests || got.Summary.UpstreamAttempts != want.Summary.UpstreamAttempts || got.Summary.Tokens != want.Summary.Tokens || got.Summary.KnownCost != want.Summary.KnownCost || got.Summary.UnpricedTokens != want.Summary.UnpricedTokens {
		t.Fatalf("summary=%+v want=%+v", got.Summary, want.Summary)
	}
	if !reflect.DeepEqual(got.Series.Points, want.Series.Points) {
		t.Fatalf("series=%+v want=%+v", got.Series.Points, want.Series.Points)
	}
	if !reflect.DeepEqual(got.Dimensions.Rows, want.Dimensions.Rows) {
		t.Fatalf("dimensions=%+v want=%+v", got.Dimensions.Rows, want.Dimensions.Rows)
	}
	if !reflect.DeepEqual(got.Leaderboard.Rows, want.Leaderboard.Rows) {
		t.Fatalf("leaderboard=%+v want=%+v", got.Leaderboard.Rows, want.Leaderboard.Rows)
	}
}

func TestC4bRetentionSQLiteQueryParity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	legacy, err := store.Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: fixturePriceBook()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = legacy.Close(ctx) }()
	events := loadFixtureEvents(t)
	if err = s.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	if err = legacy.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	base := model.Query{SchemaVersion: 1, Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), TimeZone: "UTC"}
	queries := []model.Query{base, base, base}
	queries[1].KeyIDs = []string{events[0].KeyID}
	queries[2].KeyIDs = []string{events[0].KeyID, events[1].KeyID}
	wants := []c4bQuerySnapshot{}
	for _, q := range queries {
		wants = append(wants, c4bQuery(t, legacy, q))
	}
	raw := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, daily := range []time.Time{{}, raw} {
		if _, err = s.ApplyRetentionPolicy(ctx, raw, daily, 1); err != nil {
			t.Fatal(err)
		}
		for i, q := range queries {
			c4bAssertQuery(t, c4bQuery(t, s, q), wants[i])
		}
	}
}

func TestC4bRetentionRestartAndImportRollback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handle := chtest.Handle(t)
	config := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: fixturePriceBook()}
	s, err := Open(ctx, handle, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := RunLegacyImport(ctx, s, config.Path); err != nil {
		t.Fatal(err)
	}
	events := loadFixtureEvents(t)
	if _, err = s.WriteImportBatch(ctx, events, "retained-import"); err != nil {
		t.Fatal(err)
	}
	want := c4bVisible(t, s)
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	failure := errors.New("process stopped after publication")
	interrupted := context.WithValue(ctx, c4bStepKey{}, func(step string) error {
		if step == "raw:published" {
			return failure
		}
		return nil
	})
	if _, err = s.ApplyRetention(interrupted, cutoff, 1); !errors.Is(err, failure) {
		t.Fatalf("err=%v", err)
	}
	if err = s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, handle, config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(ctx) }()
	if s.RetentionCutoff() == nil {
		t.Fatal("published retention floor lost on restart")
	}
	if got := c4bVisible(t, s); got != want {
		t.Fatalf("restart=%+v want=%+v", got, want)
	}
	if _, err = s.ApplyRetentionPolicy(ctx, cutoff, cutoff, 1); err != nil {
		t.Fatal(err)
	}
	if got := c4bVisible(t, s); got != want {
		t.Fatalf("retry=%+v want=%+v", got, want)
	}
	removed, err := s.RollbackImport(ctx, "retained-import")
	if err != nil || removed == 0 {
		t.Fatalf("rollback=%d err=%v", removed, err)
	}
	if got := c4bVisible(t, s); got != (c4bTotals{}) {
		t.Fatalf("rollback left totals=%+v", got)
	}
}
