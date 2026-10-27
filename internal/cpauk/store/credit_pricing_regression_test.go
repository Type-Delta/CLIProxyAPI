package store

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func creditRegressionEvent() model.Event {
	event := v2Event(strings.Repeat("c", 32), strings.Repeat("d", 32), strings.Repeat("a", 64), time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC), true, nil, 0, 0, 10, 5)
	event.Provider = "kiro"
	credits := "2"
	event.ProviderCredits = &credits
	return event
}

func TestCreditPricingRestoreEmptyBook(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(ctx) }()
	backup := filepath.Join(t.TempDir(), "backup.db")
	if _, err = db.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	rate := model.NanoUSD(1_000_000_000)
	if _, err = db.UpdatePriceBook(ctx, aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "manual", Provider: "kiro", Model: "model-v2", InputPerMillion: &rate, OutputPerMillion: &rate, Source: "management-api"}}}); err != nil {
		t.Fatal(err)
	}
	if err = db.Restore(ctx, backup, backup+".manifest.json"); err != nil {
		t.Fatal(err)
	}
	book, err := db.PriceBook(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(book.Rules) != 0 {
		t.Fatalf("restored durable book unexpectedly has rules: %+v", book)
	}
	event := creditRegressionEvent()
	if err = db.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	found, ok, err := db.EventByAttemptID(ctx, event.AttemptID, v2Query(model.OperationEvents, event.RequestedAt.Add(-time.Minute), event.RequestedAt.Add(time.Minute)))
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if found.KnownCost == nil || *found.KnownCost != 40_000_000 || found.PriceSource != "kiro-credits" {
		t.Fatalf("durable settings have no rules, but after restore cost=%v source=%q applied_rate=%v", found.KnownCost, found.PriceSource, found.PriceUSDPerCredit)
	}
}

func TestCreditPricingManualSourceDoesNotChangeBasis(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(ctx) }()
	rate := model.NanoUSD(1_000_000_000)
	if _, err = db.UpdatePriceBook(ctx, aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "manual", Provider: "kiro", Model: "model-v2", InputPerMillion: &rate, OutputPerMillion: &rate, Source: "kiro-credits"}}}); err != nil {
		t.Fatal(err)
	}
	event := creditRegressionEvent()
	if err = db.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	costs, err := db.analysisCosts(ctx, v2Query(model.OperationAnalysis, event.RequestedAt.Add(-time.Minute), event.RequestedAt.Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if costs.CreditBasedUSD != "0" || costs.UncachedInputUSD != "0.0001" || costs.OutputUSD != "0.0001" {
		t.Fatalf("manual token override was classified as credits: %+v", costs)
	}
}

func TestCreditPricingRoundingLimits(t *testing.T) {
	rate := model.NanoUSD(math.MaxInt64)
	book := aggregate.PriceBook{KiroUSDPerCredit: &rate}
	event := creditRegressionEvent()
	for _, tc := range []struct {
		credits  string
		want     model.NanoUSD
		overflow bool
	}{
		{"1", model.NanoUSD(math.MaxInt64), false},
		{"1.000000000000000001", 0, true},
		{"0.000000000000000001", 9, false},
		{strings.Repeat("9", 64), 0, true},
	} {
		event.ProviderCredits = &tc.credits
		result, err := book.Price(event)
		if tc.overflow {
			if err == nil {
				t.Fatalf("%s accepted overflow", tc.credits)
			}
			continue
		}
		if err != nil || result.KnownCost == nil || *result.KnownCost != tc.want {
			t.Fatalf("credits=%s result=%+v err=%v want=%v", tc.credits, result, err, tc.want)
		}
	}
	for _, tc := range []struct {
		credits string
		want    model.NanoUSD
	}{{"0.000000024999999999", 0}, {"0.000000025", 1}, {"0.000000025000000001", 1}} {
		event.ProviderCredits = &tc.credits
		result, err := (aggregate.PriceBook{}).Price(event)
		if err != nil || result.KnownCost == nil || *result.KnownCost != tc.want {
			t.Fatalf("credits=%s result=%+v err=%v want=%v", tc.credits, result, err, tc.want)
		}
	}
}

func TestReviewCreditRetentionReprice(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(ctx) }()
	event := creditRegressionEvent()
	if err = db.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	query := v2Query(model.OperationSummary, event.RequestedAt.Add(-time.Hour), event.RequestedAt.Add(23*time.Hour))
	before, err := db.Summary(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ApplyRetentionPolicy(ctx, event.RequestedAt.Add(23*time.Hour), time.Time{}, 100); err != nil {
		t.Fatal(err)
	}
	rate := model.NanoUSD(0)
	if _, err = db.UpdatePriceBook(ctx, aggregate.PriceBook{KiroUSDPerCredit: &rate}); err != nil {
		t.Fatal(err)
	}
	result, err := db.Reprice(ctx, RepriceOptions{Range: model.Range{Start: query.Start, End: query.End, TimeZone: "UTC"}}, nil)
	if err != nil || result.Updated != 0 {
		t.Fatalf("reprice retained data: %+v %v", result, err)
	}
	after, err := db.Summary(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if after.KnownCost != before.KnownCost || after.KnownCost != 40_000_000 || after.Tokens != before.Tokens {
		t.Fatalf("retained summary changed: before=%+v after=%+v", before, after)
	}
}

func TestReviewCreditAliasPrecedence(t *testing.T) {
	event := creditRegressionEvent()
	alias := "alias-model"
	event.RequestedAlias = &alias
	tokenRate := model.NanoUSD(1_000_000_000)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{
		{ID: "catalog", Provider: "kiro", Model: event.Model, Catalog: true, InputPerMillion: &tokenRate, OutputPerMillion: &tokenRate, Source: "models.dev"},
		{ID: "manual-alias", Alias: alias, InputPerMillion: &tokenRate, OutputPerMillion: &tokenRate, Source: "management-api"},
	}}
	price, err := book.Price(event)
	if err != nil || price.KnownCost == nil || *price.KnownCost != 200_000 || price.PriceUSDPerCredit != nil {
		t.Fatalf("known alias override: %+v %v", price, err)
	}
	book.Rules[1].InputPerMillion, book.Rules[1].OutputPerMillion = nil, nil
	price, err = book.Price(event)
	if err != nil || price.KnownCost != nil || price.UnpricedTokens != 200 || price.PriceUSDPerCredit != nil {
		t.Fatalf("unknown alias override: %+v %v", price, err)
	}
	book.Rules = book.Rules[:1]
	price, err = book.Price(event)
	if err != nil || price.KnownCost == nil || *price.KnownCost != 40_000_000 || price.PriceUSDPerCredit == nil {
		t.Fatalf("credits before catalog: %+v %v", price, err)
	}
	event.ProviderCredits = nil
	price, err = book.Price(event)
	if err != nil || price.KnownCost == nil || *price.KnownCost != 200_000 || price.PriceUSDPerCredit != nil {
		t.Fatalf("unmetered catalog fallback: %+v %v", price, err)
	}
}
