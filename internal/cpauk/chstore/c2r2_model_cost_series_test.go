package chstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestModelCostSeriesTiersComponentsAndFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	input, output := model.NanoUSD(1_000_000_000), model.NanoUSD(2_000_000_000)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "tiers", Model: "model-v2", InputPerMillion: &input, OutputPerMillion: &output, CacheReadMultiplier: "0.1", CacheCreationMultiplier: "1.25", Source: "test", ServiceTierPrices: map[string]aggregate.ServiceTierPrice{
		"priority": {InputPerMillion: 2_000_000_000, OutputPerMillion: 4_000_000_000, CacheReadMultiplier: "0.1", CacheCreationMultiplier: "1.25"},
		"flex":     {InputPerMillion: 500_000_000, OutputPerMillion: 1_000_000_000, CacheReadMultiplier: "0.1", CacheCreationMultiplier: "1.25"},
	}}}}
	db := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "series.db"), MaxStorageBytes: 64 << 20, PriceBook: book})
	t.Cleanup(func() { _ = db.Close(ctx) })
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	priority, standard, flex := "priority", "default", "flex"
	tiers := []*string{&standard, &priority, &priority, &flex, &priority, &priority}
	events := make([]model.Event, len(tiers))
	for i, tier := range tiers {
		events[i] = c2r2V2Event(fmt.Sprintf("%032x", i+1), fmt.Sprintf("%032x", i+100), strings.Repeat("a", 64), start.Add(time.Duration(i+1)*time.Minute), true, nil, 20, 20, 10, 20)
		events[i].Tokens.CacheCreation = 10
		events[i].ServiceTierRequested, events[i].ServiceTierUsed = &priority, tier
	}
	events[4].Provider = "other"
	events[5].KeyID = strings.Repeat("b", 64)
	if err := db.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	query := c2r2V2Query(model.OperationModelCostSeries, start, start.Add(2*time.Hour))
	query.BucketWidth = "1h"
	query.KeyIDs = []string{strings.Repeat("a", 64)}
	query.Filters = map[string]json.RawMessage{"model": json.RawMessage(`["model-v2"]`), "provider": json.RawMessage(`["provider-v2"]`)}
	result, err := db.ModelCostSeries(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if result.Partial || len(result.Buckets) != 2 || len(result.Buckets[0].Tiers) != 3 || len(result.Buckets[1].Tiers) != 0 {
		t.Fatalf("series shape = %+v", result)
	}
	var tokens int64
	byTier := map[string]model.ModelCostSeriesTier{}
	for _, tier := range result.Buckets[0].Tiers {
		tokens += tier.TotalTokens
		byTier[tier.ServiceTier] = tier
		var sum model.NanoUSD
		for _, component := range tier.Costs {
			if component == nil {
				t.Fatal("missing priced component")
			}
			sum += *component
		}
		if sum != tier.KnownCost {
			t.Fatalf("components %s != total %s", sum, tier.KnownCost)
		}
	}
	if tokens != 800 {
		t.Fatalf("filtered total = %d, want 800", tokens)
	}
	p := byTier["priority"]
	if p.InputTokens != 140 || p.CacheReadTokens != 40 || p.CacheCreationTokens != 20 || p.OutputTokens != 200 {
		t.Fatalf("priority tokens = %+v", p)
	}
	if p.KnownCost != 1_138_000 || byTier["default"].KnownCost != 284_500 || byTier["flex"].KnownCost != 142_250 {
		t.Fatalf("tier costs = %+v", byTier)
	}
	if p.Rates[0] == nil || *p.Rates[0] != "2" {
		t.Fatalf("priority rates = %+v", p.Rates)
	}
	// Range and service-tier filters use the same event selection as all analytics reads.
	query.Start = start.Add(3 * time.Minute)
	query.Filters["service_tier"] = json.RawMessage(`["flex"]`)
	result, err = db.ModelCostSeries(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Buckets[0].Tiers) != 1 || result.Buckets[0].Tiers[0].ServiceTier != "flex" {
		t.Fatalf("filtered tiers = %+v", result)
	}
	query.Filters["model"] = json.RawMessage(`["other-model"]`)
	result, err = db.ModelCostSeries(ctx, query)
	if err != nil || len(result.Buckets[0].Tiers) != 0 {
		t.Fatalf("model filter = %+v, %v", result, err)
	}
}

func TestModelCostSeriesValidationAndTimeZone(t *testing.T) {
	t.Parallel()
	db, events := c2r2OpenV2FixtureStore(t)
	q := c2r2V2Query(model.OperationModelCostSeries, events[0].RequestedAt, events[0].RequestedAt.Add(time.Hour))
	if _, err := db.ModelCostSeries(context.Background(), q); err == nil {
		t.Fatal("missing model accepted")
	}
	q.Filters = map[string]json.RawMessage{"model": json.RawMessage(`["model-v2","other"]`)}
	if _, err := db.ModelCostSeries(context.Background(), q); err == nil {
		t.Fatal("multiple models accepted")
	}
	q.Filters["model"] = json.RawMessage(`["model-v2"]`)
	q.SchemaVersion = 1
	if err := q.Validate(); err == nil {
		t.Fatal("v1 accepted")
	}
	q.SchemaVersion = 2
	q.TimeZone, q.BucketWidth = "Asia/Bangkok", "1d"
	result, err := db.ModelCostSeries(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 31, 17, 0, 0, 0, time.UTC)
	if !result.Buckets[0].Start.Equal(want) {
		t.Fatalf("bucket start = %s, want %s", result.Buckets[0].Start, want)
	}
	db.mu.Lock()
	db.retentionCutoff = q.Start.Add(time.Minute)
	db.mu.Unlock()
	result, err = db.ModelCostSeries(context.Background(), q)
	if err != nil || !result.Partial {
		t.Fatalf("retention coverage = %+v, %v", result, err)
	}
}

func TestModelCostSeriesPreservesRecordedRatesAcrossCatalogChanges(t *testing.T) {
	t.Parallel()
	db, events := c2r2OpenV2FixtureStore(t)
	ctx := context.Background()
	query := c2r2V2Query(model.OperationModelCostSeries, events[0].RequestedAt, events[0].RequestedAt.Add(time.Hour))
	query.Filters = map[string]json.RawMessage{"model": json.RawMessage(`["model-v2"]`)}
	before, err := db.ModelCostSeries(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	input, output := model.NanoUSD(5_000_000_000), model.NanoUSD(1_000_000_000)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{
		ID: "v2-price", Model: "model-v2", InputPerMillion: &input, OutputPerMillion: &output,
		CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "v2-fixture",
	}}}
	if _, err := db.UpdatePriceBook(ctx, book); err != nil {
		t.Fatal(err)
	}
	after, err := db.ModelCostSeries(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	left, _ := json.Marshal(before.Buckets)
	right, _ := json.Marshal(after.Buckets)
	if string(left) != string(right) || after.Partial {
		t.Fatalf("catalog change rewrote historical pricing: before=%s after=%s partial=%t", left, right, after.Partial)
	}
}

func TestModelCostSeriesLegacyReusesCompletePricingBreakdown(t *testing.T) {
	t.Parallel()
	db, events := c2r2OpenV2FixtureStore(t)
	ctx := context.Background()
	query := c2r2V2Query(model.OperationModelCostSeries, events[0].RequestedAt, events[0].RequestedAt.Add(time.Hour))
	query.Filters = map[string]json.RawMessage{"model": json.RawMessage(`["model-v2"]`)}
	before, err := db.ModelCostSeries(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.db.Conn().Exec(ctx, "ALTER TABLE "+db.table("analytics_events")+" UPDATE pricing_breakdown=NULL WHERE 1 SETTINGS mutations_sync=2"); err != nil {
		t.Fatal(err)
	}
	after, err := db.ModelCostSeries(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	left, _ := json.Marshal(before)
	right, _ := json.Marshal(after)
	if string(left) != string(right) {
		t.Fatalf("legacy reconstruction differs from recorded breakdown: before=%s after=%s", left, right)
	}
}

type c2r2CancelModelCostAfterChecks struct {
	context.Context
	cancel context.CancelFunc
	checks atomic.Int64
}

func (c *c2r2CancelModelCostAfterChecks) Err() error {
	// SQL returns chart groups, so cancel during the first aggregated scan.
	if c.checks.Add(1) >= 3 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestModelCostSeriesChecksCancellationDuringScan(t *testing.T) {
	t.Parallel()
	db, events := c2r2OpenV2FixtureStore(t)
	for i := 0; i < 50; i++ {
		event := events[0]
		event.AttemptID, event.ProxyRequestID = fmt.Sprintf("%032x", i+100), fmt.Sprintf("%032x", i+100)
		if err := db.WriteBatch(context.Background(), []model.Event{event}); err != nil {
			t.Fatal(err)
		}
	}
	query := c2r2V2Query(model.OperationModelCostSeries, events[0].RequestedAt, events[0].RequestedAt.Add(time.Hour))
	query.Filters = map[string]json.RawMessage{"model": json.RawMessage(`["model-v2"]`)}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &c2r2CancelModelCostAfterChecks{Context: base, cancel: cancel}
	_, err := db.ModelCostSeries(ctx, query)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan did not observe cancellation: %v (checks=%d)", err, ctx.checks.Load())
	}
}
