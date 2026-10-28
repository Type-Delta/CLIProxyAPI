package chstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestPricingCatalogValidationIsBoundedAndTransactional(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openTestStore(t)
	input, output := model.NanoUSD(1), model.NanoUSD(2)
	provenance := PricingProvenance{Source: "fixture", SourceDigest: strings.Repeat("c", 64), SyncedAt: time.Now().UTC()}
	valid := []aggregate.PricingRule{{ID: "valid", Model: "model", InputPerMillion: &input,
		OutputPerMillion: &output, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "fixture"}}
	if err := database.ReplacePricingRules(ctx, valid, provenance); err != nil {
		t.Fatal(err)
	}
	invalid := append([]aggregate.PricingRule(nil), valid...)
	invalid[0].OutputPerMillion = nil
	if err := database.ReplacePricingRules(ctx, invalid, provenance); err == nil {
		t.Fatal("invalid catalog was accepted")
	}
	tooMany := make([]aggregate.PricingRule, MaxPricingRules+1)
	if err := database.ReplacePricingRules(ctx, tooMany, provenance); err == nil {
		t.Fatal("oversized catalog was accepted")
	}
	snapshot, err := database.PricingSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Rules) != 1 || snapshot.Rules[0].ID != "valid" {
		t.Fatalf("failed update changed catalog: %+v", snapshot)
	}
}

func TestC3InvalidManualPricesPreserveBook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	rate := model.NanoUSD(1)
	valid := aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "valid", Model: "m", InputPerMillion: &rate, OutputPerMillion: &rate, Source: "test"}}}
	if _, err := s.UpdatePriceBook(ctx, valid); err != nil {
		t.Fatal(err)
	}
	bad := valid
	bad.Rules = append([]aggregate.PricingRule(nil), valid.Rules...)
	bad.Rules[0].OutputPerMillion = nil
	if _, err := s.UpdatePriceBook(ctx, bad); err == nil {
		t.Fatal("invalid rule accepted")
	}
	got, err := s.PriceBook(ctx)
	if err != nil || len(got.Rules) != 1 || got.Rules[0].OutputPerMillion == nil {
		t.Fatalf("book changed %+v %v", got, err)
	}
}
