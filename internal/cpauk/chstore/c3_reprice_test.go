package chstore

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

func TestC3RepriceResumeRestartAndCatalogChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "a.db")
	s := openDiscoveryStore(t, path, nil, &now)
	events := []model.Event{c3V2Event(strings.Repeat("a", 32), strings.Repeat("1", 32), strings.Repeat("d", 64), now.Add(time.Minute), true, nil, 0, 0, 10, 20), c3V2Event(strings.Repeat("b", 32), strings.Repeat("2", 32), strings.Repeat("e", 64), now.Add(2*time.Minute), true, nil, 0, 0, 10, 20)}
	if err := s.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	rate := model.NanoUSD(1_000_000_000)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "price", Model: "model-v2", InputPerMillion: &rate, OutputPerMillion: &rate, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "test"}}}
	if _, err := s.UpdatePriceBook(ctx, book); err != nil {
		t.Fatal(err)
	}
	selected := model.Range{Start: now, End: now.Add(time.Hour), TimeZone: "UTC"}
	dry, err := s.Reprice(ctx, RepriceOptions{Range: selected, ChunkSize: 1, DryRun: true}, nil)
	if err != nil || dry.Matched != 2 || dry.Updated != 0 || dry.Completed {
		t.Fatalf("dry run %+v %v", dry, err)
	}
	first, err := s.Reprice(ctx, RepriceOptions{Range: selected, ChunkSize: 1}, nil)
	if err != nil || first.Updated != 1 || first.Completed {
		t.Fatalf("first %+v %v", first, err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s = openDiscoveryStore(t, path, nil, &now)
	stored, found, err := s.metadata(ctx, repriceCheckpointKey(selected))
	if err != nil || !found || stored != first.Checkpoint {
		t.Fatalf("checkpoint %q %v %v", stored, found, err)
	}
	result, err := s.Reprice(ctx, RepriceOptions{Range: selected, ChunkSize: 1, Resume: true}, nil)
	if err != nil || result.Updated != 1 || !result.Completed {
		t.Fatalf("resume %+v %v", result, err)
	}
	_, found, err = s.metadata(ctx, repriceCheckpointKey(selected))
	if err != nil || found {
		t.Fatalf("cleared checkpoint %v %v", found, err)
	}
	rate = 2_000_000_000
	if _, err := s.UpdatePriceBook(ctx, book); err != nil {
		t.Fatal(err)
	}
	_, err = s.Reprice(ctx, RepriceOptions{Range: selected, ResumeCheckpoint: first.Checkpoint}, nil)
	if !errors.Is(err, ErrRepriceCatalogChanged) {
		t.Fatalf("changed catalog %v", err)
	}
}

func TestC3RepriceSurvivingPartialBucketMatchesSQLite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	start := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	old := model.NanoUSD(1_000_000_000)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "old", Model: "model-v2", InputPerMillion: &old, OutputPerMillion: &old, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "test"}}}
	events := []model.Event{c3V2Event(strings.Repeat("1", 32), strings.Repeat("a", 32), strings.Repeat("d", 64), start.Add(time.Minute), true, nil, 0, 0, 10, 20), c3V2Event(strings.Repeat("2", 32), strings.Repeat("b", 32), strings.Repeat("e", 64), start.Add(time.Hour+time.Minute), true, nil, 0, 0, 10, 20)}
	ch := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "ch.db"), MaxStorageBytes: 64 << 20, PriceBook: book})
	sq, err := store.Open(ctx, store.Config{Path: filepath.Join(t.TempDir(), "sq.db"), MaxStorageBytes: 64 << 20, PriceBook: book})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sq.Close(ctx) }()
	for _, db := range []interface {
		WriteBatch(context.Context, []model.Event) error
		ApplyRetention(context.Context, time.Time, int) (RetentionResult, error)
		UpdatePriceBook(context.Context, aggregate.PriceBook) (aggregate.PriceBook, error)
		Reprice(context.Context, RepriceOptions, func(int, string)) (RepriceResult, error)
	}{ch, sq} {
		if err := db.WriteBatch(ctx, events); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ApplyRetention(ctx, start.Add(90*time.Minute), 100); err != nil {
			t.Fatal(err)
		}
		rate := model.NanoUSD(2_000_000_000)
		if _, err := db.UpdatePriceBook(ctx, aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "new", Model: "model-v2", InputPerMillion: &rate, OutputPerMillion: &rate, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "test"}}}); err != nil {
			t.Fatal(err)
		}
		result, err := db.Reprice(ctx, RepriceOptions{Range: model.Range{Start: start, End: start.Add(2 * time.Hour), TimeZone: "UTC"}}, nil)
		if err != nil || result.Matched != 1 || result.Updated != 1 || result.HistoryComplete {
			t.Fatalf("partial-bucket reprice %+v %v", result, err)
		}
	}
	got, err := ch.loadEvents(ctx, "attempt_id=?", events[1].AttemptID)
	if err != nil || len(got) != 1 || got[0].KnownCost == nil || *got[0].KnownCost != 400000 {
		t.Fatalf("surviving raw %+v %v", got, err)
	}
}
