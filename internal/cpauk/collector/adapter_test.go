package collector

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestAdapterServiceTierPricingPrecedence(t *testing.T) {
	base := model.NanoUSD(1_000_000_000)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{
		ID: "catalog", Provider: "codex", Model: "model", Source: "models.dev", Catalog: true,
		InputPerMillion: &base, OutputPerMillion: &base,
		ServiceTierPrices: map[string]aggregate.ServiceTierPrice{
			"priority": {InputPerMillion: 2_000_000_000, OutputPerMillion: 2_000_000_000},
		},
	}}}
	for _, tc := range []struct {
		name, client, effective, returned, detailReturned, wantUsed, wantTier string
		wantCost                                                              model.NanoUSD
	}{
		{"outbound override", "auto", "priority", "", "", "priority", "priority", 400_000},
		{"returned tier wins", "auto", "priority", "default", "priority", "default", "default", 200_000},
		{"detail returned tier wins", "auto", "priority", "", "default", "default", "default", 200_000},
		{"client fallback", "priority", "", "", "", "", "priority", 400_000},
		{"no tier", "", "", "", "", "", "", 200_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := validRecord()
			record.Provider, record.Model = "codex", "model"
			record.ServiceTier, record.EffectiveServiceTier = tc.client, tc.effective
			record.ResponseServiceTier, record.Detail.ResponseServiceTier = tc.returned, tc.detailReturned
			record.Detail.InputTokens, record.Detail.OutputTokens, record.Detail.TotalTokens = 100, 100, 200
			sanitizer := NewSanitizer(SanitizerOptions{})
			intake := &fakeIntake{generation: 1}
			NewAdapter(intake, sanitizer).HandleUsage(context.Background(), record)
			if len(intake.events) != 1 || intake.rejected != 0 {
				t.Fatalf("collector intake = %+v, want one accepted event", intake)
			}
			event := intake.events[0]
			if got := tierValue(event.ServiceTierRequested); got != tc.client {
				t.Fatalf("client preference = %q, want %q", got, tc.client)
			}
			if got := tierValue(event.ServiceTierUsed); got != tc.wantUsed {
				t.Fatalf("used tier = %q, want %q", got, tc.wantUsed)
			}
			price, err := book.Price(event)
			if err != nil {
				t.Fatal(err)
			}
			if price.KnownCost == nil || *price.KnownCost != tc.wantCost {
				t.Fatalf("price = %+v, want %d nano-USD", price, tc.wantCost)
			}
			if price.Breakdown == nil || price.Breakdown.ServiceTier != tc.wantTier {
				t.Fatalf("pricing snapshot = %+v, want tier %q", price.Breakdown, tc.wantTier)
			}
		})
	}
}

func tierValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
