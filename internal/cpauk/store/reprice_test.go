package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func TestRepriceDryRunCommitAndResumeCheckpoint(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	start := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	events := []model.Event{
		v2Event("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "11111111111111111111111111111111", "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", start.Add(time.Minute), true, nil, 0, 0, 10, 20),
		v2Event("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "22222222222222222222222222222222", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", start.Add(2*time.Minute), true, nil, 0, 0, 10, 20),
	}
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	input, output := model.NanoUSD(1_000_000_000), model.NanoUSD(1_000_000_000)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "reprice-v2", Model: "model-v2", InputPerMillion: &input,
		OutputPerMillion: &output, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "management-api"}}}
	if _, err := database.UpdatePriceBook(ctx, book); err != nil {
		t.Fatal(err)
	}
	options := RepriceOptions{Range: model.Range{Start: start, End: start.Add(time.Hour), TimeZone: "UTC"}, DryRun: true, ChunkSize: 1}
	dryRun, err := database.Reprice(ctx, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dryRun.Matched != 2 || dryRun.Updated != 0 || dryRun.Checkpoint == "" || dryRun.Completed {
		t.Fatalf("dry-run result = %+v", dryRun)
	}
	assertStoredPricing(t, database, events[0].AttemptID, false)

	options.DryRun = false
	firstChunk, err := database.Reprice(ctx, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	if firstChunk.Updated != 1 || firstChunk.Checkpoint == "" || firstChunk.Completed {
		t.Fatalf("first reprice chunk = %+v", firstChunk)
	}
	options.ResumeCheckpoint = firstChunk.Checkpoint
	secondChunk, err := database.Reprice(ctx, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	if secondChunk.Updated != 1 || !secondChunk.Completed {
		t.Fatalf("resumed reprice = %+v", secondChunk)
	}
	for _, event := range events {
		assertStoredPricing(t, database, event.AttemptID, true)
	}
}

func TestRepriceResumeRejectsChangedPricingCatalog(t *testing.T) {
	ctx := context.Background()
	database, events := openV2FixtureStore(t)
	selected := model.Range{Start: events[0].RequestedAt.Add(-time.Minute), End: events[len(events)-1].RequestedAt.Add(time.Minute), TimeZone: "UTC"}
	first, err := database.Reprice(ctx, RepriceOptions{Range: selected, ChunkSize: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	changedRate := model.NanoUSD(2_000_000_000)
	if _, err := database.UpdatePriceBook(ctx, aggregate.PriceBook{Rules: []aggregate.PricingRule{{
		ID: "changed", Model: "model-v2", InputPerMillion: &changedRate, OutputPerMillion: &changedRate,
		CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "changed",
	}}}); err != nil {
		t.Fatal(err)
	}
	_, err = database.Reprice(ctx, RepriceOptions{Range: selected, ChunkSize: 1, ResumeCheckpoint: first.Checkpoint}, nil)
	if !errors.Is(err, ErrRepriceCatalogChanged) {
		t.Fatalf("resume after catalog change error = %v, want ErrRepriceCatalogChanged", err)
	}
}

func TestRepriceAppliesRawSuffixAndReportsRetainedCutoff(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	oldRate := model.NanoUSD(1_000_000_000)
	database, err := Open(ctx, Config{
		Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20,
		PriceBook: aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "old", Model: "model-v2", InputPerMillion: &oldRate, OutputPerMillion: &oldRate, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "test"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	events := []model.Event{
		v2Event(strings.Repeat("1", 32), strings.Repeat("a", 32), strings.Repeat("d", 64), start.Add(time.Minute), true, nil, 0, 0, 10, 20),
		v2Event(strings.Repeat("2", 32), strings.Repeat("b", 32), strings.Repeat("e", 64), start.Add(time.Hour+time.Minute), true, nil, 0, 0, 10, 20),
	}
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	// The cutoff falls inside the second hour. Retention removes the complete
	// first hour but leaves the second event raw even though it precedes the
	// reported exact-history cutoff.
	cutoff := start.Add(90 * time.Minute)
	if _, err := database.ApplyRetention(ctx, cutoff, 100); err != nil {
		t.Fatal(err)
	}
	newRate := model.NanoUSD(2_000_000_000)
	if _, err := database.UpdatePriceBook(ctx, aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "new", Model: "model-v2", InputPerMillion: &newRate, OutputPerMillion: &newRate, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "test"}}}); err != nil {
		t.Fatal(err)
	}

	result, err := database.Reprice(ctx, RepriceOptions{Range: model.Range{Start: start, End: start.Add(2 * time.Hour), TimeZone: "UTC"}, ChunkSize: 100}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.HistoryComplete || result.RetainedCutoff == nil || !result.RetainedCutoff.Equal(cutoff) || !result.EffectiveStart.Equal(cutoff) || result.Matched != 1 || result.Updated != 1 {
		t.Fatalf("reprice retained coverage = %+v", result)
	}
	var rawCost int64
	var rawRule, rawSource string
	if err := database.db.QueryRowContext(ctx, "SELECT known_cost_nano,price_rule_id,price_source FROM events WHERE attempt_id=?", events[1].AttemptID).Scan(&rawCost, &rawRule, &rawSource); err != nil {
		t.Fatal(err)
	}
	if rawCost != 400_000 || rawRule != "new" || rawSource != "test" {
		t.Fatalf("repriced raw event = cost %d rule %q source %q", rawCost, rawRule, rawSource)
	}
	var retainedCost int64
	if err := database.db.QueryRowContext(ctx, "SELECT SUM(known_cost_nano) FROM rollups").Scan(&retainedCost); err != nil {
		t.Fatal(err)
	}
	if retainedCost != 200_000 {
		t.Fatalf("retained cost = %d, want original 200000", retainedCost)
	}
}

func TestRepriceCheckpointCatalogDigestSurvivesRestartOrdering(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	config := Config{Path: filepath.Join(directory, "analytics.db"), MaxStorageBytes: 64 << 20}
	database, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	events := []model.Event{
		v2Event(strings.Repeat("7", 32), strings.Repeat("8", 32), strings.Repeat("c", 64), start.Add(time.Minute), true, nil, 0, 0, 1, 10),
		v2Event(strings.Repeat("9", 32), strings.Repeat("a", 32), strings.Repeat("d", 64), start.Add(2*time.Minute), true, nil, 0, 0, 1, 10),
	}
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	rate := model.NanoUSD(1_000_000_000)
	unsorted := aggregate.PriceBook{Rules: []aggregate.PricingRule{
		{ID: "z-rule", Model: "model-v2", InputPerMillion: &rate, OutputPerMillion: &rate, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "test"},
		{ID: "a-rule", Model: "other-model", InputPerMillion: &rate, OutputPerMillion: &rate, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "test"},
	}}
	if _, err := database.UpdatePriceBook(ctx, unsorted); err != nil {
		t.Fatal(err)
	}
	selected := model.Range{Start: start, End: start.Add(time.Hour), TimeZone: "UTC"}
	first, err := database.Reprice(ctx, RepriceOptions{Range: selected, ChunkSize: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	resumed, err := database.Reprice(ctx, RepriceOptions{Range: selected, ChunkSize: 1, ResumeCheckpoint: first.Checkpoint}, nil)
	if err != nil {
		t.Fatalf("resume after restart: %v", err)
	}
	if !resumed.Completed || resumed.Updated != 1 {
		t.Fatalf("resumed reprice = %+v", resumed)
	}
}

func TestRepriceResumeLoadsPersistedCheckpointAndClearsItOnCompletion(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	start := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	events := []model.Event{
		v2Event("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "11111111111111111111111111111111", "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", start.Add(time.Minute), true, nil, 0, 0, 10, 20),
		v2Event("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "22222222222222222222222222222222", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", start.Add(2*time.Minute), true, nil, 0, 0, 10, 20),
	}
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	input, output := model.NanoUSD(1_000_000_000), model.NanoUSD(1_000_000_000)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{
		ID: "reprice-resume", Model: "model-v2", InputPerMillion: &input, OutputPerMillion: &output,
		CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "management-api",
	}}}
	if _, err := database.UpdatePriceBook(ctx, book); err != nil {
		t.Fatal(err)
	}
	selected := model.Range{Start: start, End: start.Add(time.Hour), TimeZone: "UTC"}
	first, err := database.Reprice(ctx, RepriceOptions{Range: selected, ChunkSize: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Completed || first.Checkpoint == "" || first.Updated != 1 {
		t.Fatalf("first reprice chunk = %+v", first)
	}
	var stored string
	if err := database.db.QueryRowContext(ctx, "SELECT value FROM analytics_metadata WHERE key=?", repriceCheckpointKey(selected)).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != first.Checkpoint {
		t.Fatalf("persisted checkpoint = %q, want %q", stored, first.Checkpoint)
	}
	resumed, err := database.Reprice(ctx, RepriceOptions{Range: selected, ChunkSize: 1, Resume: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Completed || resumed.Updated != 1 {
		t.Fatalf("resumed reprice = %+v", resumed)
	}
	var checkpointCount int
	if err := database.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM analytics_metadata WHERE key=?", repriceCheckpointKey(selected)).Scan(&checkpointCount); err != nil {
		t.Fatal(err)
	}
	if checkpointCount != 0 {
		t.Fatalf("completed checkpoint rows = %d", checkpointCount)
	}
}

func assertStoredPricing(t *testing.T, database *SQLiteStore, attemptID string, priced bool) {
	t.Helper()
	var knownCost any
	var unpriced int64
	var ruleID, source any
	if err := database.db.QueryRowContext(context.Background(), `SELECT known_cost_nano,unpriced_tokens,price_rule_id,price_source
FROM events WHERE attempt_id=?`, attemptID).Scan(&knownCost, &unpriced, &ruleID, &source); err != nil {
		t.Fatal(err)
	}
	if !priced {
		if knownCost != nil || unpriced != 200 || ruleID != nil || source != nil {
			t.Fatalf("dry run changed event pricing: cost=%v unpriced=%d rule=%v source=%v", knownCost, unpriced, ruleID, source)
		}
		return
	}
	if knownCost == nil || unpriced != 0 || ruleID != "reprice-v2" || source != "management-api" {
		t.Fatalf("committed pricing: cost=%v unpriced=%d rule=%v source=%v", knownCost, unpriced, ruleID, source)
	}
}
