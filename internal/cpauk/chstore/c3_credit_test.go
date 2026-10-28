package chstore

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

func TestC3CreditPricingRestartAndReprice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "a.db")
	f := &testPricingFetcher{catalog: testPricingCatalog(1_000_000_000, 2_000_000_000)}
	s := openDiscoveryStore(t, path, f, &now)
	rate := model.NanoUSD(30_000_000)
	if _, err := s.UpdatePriceBook(ctx, aggregate.PriceBook{KiroUSDPerCredit: &rate}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshPricing(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s = openDiscoveryStore(t, path, f, &now)
	book, err := s.PriceBook(ctx)
	if err != nil || book.EffectiveKiroRate() != rate {
		t.Fatalf("restart %+v %v", book, err)
	}
	if _, err := s.UpdatePriceBook(ctx, aggregate.PriceBook{}); err != nil {
		t.Fatal(err)
	}
	book, err = s.PriceBook(ctx)
	if err != nil || book.EffectiveKiroRate() != rate {
		t.Fatalf("legacy update %+v %v", book, err)
	}
	event := loadFixtureEvents(t)[0]
	event.RequestedAt = now.Add(time.Minute)
	event.Provider = "kiro"
	credits := "1.25"
	event.ProviderCredits = &credits
	if err := s.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.loadEvents(ctx, "attempt_id=?", event.AttemptID)
	if err != nil || len(rows) != 1 || rows[0].KnownCost == nil || *rows[0].KnownCost != 37_500_000 {
		t.Fatalf("stored %+v %v", rows, err)
	}
	zero := model.NanoUSD(0)
	if _, err := s.UpdatePriceBook(ctx, aggregate.PriceBook{KiroUSDPerCredit: &zero}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Reprice(ctx, RepriceOptions{Range: model.Range{Start: now, End: now.Add(time.Hour), TimeZone: "UTC"}}, nil)
	if err != nil || result.Updated != 1 {
		t.Fatalf("reprice %+v %v", result, err)
	}
	rows, err = s.loadEvents(ctx, "attempt_id=?", event.AttemptID)
	if err != nil || rows[0].KnownCost == nil || *rows[0].KnownCost != 0 || rows[0].PriceUSDPerCredit == nil || *rows[0].PriceUSDPerCredit != 0 {
		t.Fatalf("zero price %+v %v", rows, err)
	}
}
func TestC3CreditRoundingAndOverflow(t *testing.T) {
	t.Parallel()
	rate := model.NanoUSD(math.MaxInt64)
	book := aggregate.PriceBook{KiroUSDPerCredit: &rate}
	event := model.Event{Provider: "kiro"}
	for _, test := range []struct {
		credits  string
		want     model.NanoUSD
		overflow bool
	}{{"1", model.NanoUSD(math.MaxInt64), false}, {"1.000000000000000001", 0, true}, {"0.000000000000000001", 9, false}, {strings.Repeat("9", 64), 0, true}} {
		event.ProviderCredits = &test.credits
		got, err := book.Price(event)
		if test.overflow {
			if err == nil {
				t.Fatalf("accepted %s", test.credits)
			}
			continue
		}
		if err != nil || got.KnownCost == nil || *got.KnownCost != test.want {
			t.Fatalf("%s %+v %v", test.credits, got, err)
		}
	}
}

func c3CreditRegressionEvent() model.Event {
	event := c3V2Event(strings.Repeat("c", 32), strings.Repeat("d", 32), strings.Repeat("a", 64), time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC), true, nil, 0, 0, 10, 5)
	event.Provider = "kiro"
	credits := "2"
	event.ProviderCredits = &credits
	return event
}
func TestReviewCreditAliasPrecedence(t *testing.T) {
	t.Parallel()
	event := c3CreditRegressionEvent()
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
