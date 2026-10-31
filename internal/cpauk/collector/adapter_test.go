package collector

import (
	"context"
	"encoding/json"
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
		assume                                                                bool
		provider                                                              string
	}{
		{"outbound override", "auto", "priority", "", "", "priority", "priority", 400_000, false, "codex"},
		{"returned tier wins", "auto", "priority", "default", "priority", "default", "default", 200_000, false, "codex"},
		{"detail returned tier wins", "auto", "priority", "", "default", "default", "default", 200_000, false, "codex"},
		{"client fallback", "priority", "", "", "", "", "priority", 400_000, false, "codex"},
		{"no tier", "", "", "", "", "", "", 200_000, false, "codex"},
		{"assume outbound tier", "auto", "priority", "default", "default", "priority", "priority", 400_000, true, "codex"},
		{"assume without outbound retains response", "priority", "", "default", "", "default", "default", 200_000, true, "codex"},
		{"other provider unchanged", "auto", "priority", "default", "", "default", "default", 200_000, true, "openai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := validRecord()
			record.Provider, record.Model = tc.provider, "model"
			book := book
			book.Rules = append([]aggregate.PricingRule(nil), book.Rules...)
			book.Rules[0].Provider = tc.provider
			record.ServiceTier, record.EffectiveServiceTier = tc.client, tc.effective
			record.ResponseServiceTier, record.Detail.ResponseServiceTier = tc.returned, tc.detailReturned
			record.Detail.InputTokens, record.Detail.OutputTokens, record.Detail.TotalTokens = 100, 100, 200
			record.Detail.RawUsage = `{"response":{"service_tier":"default","usage":{"input_tokens":100,"output_tokens":100}}}`
			sanitizer := NewSanitizer(SanitizerOptions{})
			intake := &fakeIntake{generation: 1}
			adapter := NewAdapter(intake, sanitizer)
			adapter.SetAssumeCodexServiceTier(tc.assume)
			adapter.HandleUsage(context.Background(), record)
			if len(intake.events) != 1 || intake.rejected != 0 {
				t.Fatalf("collector intake = %+v, want one accepted event", intake)
			}
			event := intake.events[0]
			var raw struct {
				Response struct {
					ServiceTier string `json:"service_tier"`
				} `json:"response"`
			}
			if event.UpstreamUsageRaw == nil || json.Unmarshal([]byte(*event.UpstreamUsageRaw), &raw) != nil || raw.Response.ServiceTier != "default" {
				t.Fatalf("raw returned tier was lost: %v", event.UpstreamUsageRaw)
			}
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
