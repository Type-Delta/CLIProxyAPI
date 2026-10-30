package chstore

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestS5AnalysisParity(t *testing.T) {
	ctx := context.Background()
	rate, output := model.NanoUSD(400001), model.NanoUSD(700003)
	precise := model.NanoUSD(9000000000000000)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{
		{ID: "fractional", Model: "model-v2", InputPerMillion: &rate, OutputPerMillion: &output, CacheReadMultiplier: "1/3", CacheCreationMultiplier: "5/7", Source: "manual", ServiceTierPrices: map[string]aggregate.ServiceTierPrice{"priority": {InputPerMillion: 800003, OutputPerMillion: 900007, CacheReadMultiplier: "0.1", CacheCreationMultiplier: "1.25"}}},
		{ID: "precise", Model: "precise", InputPerMillion: &precise, OutputPerMillion: &precise, Source: "manual"},
		{ID: "slow", Model: "slow", InputPerMillion: &rate, OutputPerMillion: &output, CacheReadMultiplier: "1/10000000000000000000000000000000000000001", Source: "manual"},
		{ID: "alias", Alias: "alias", InputPerMillion: &output, OutputPerMillion: &rate, CacheReadMultiplier: "1/3", CacheCreationMultiplier: "1", Source: "catalog", Catalog: true},
	}}
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 256 << 20, PriceBook: book})
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	events := make([]model.Event, 300)
	for i := range events {
		e := c2r2V2Event(fmt.Sprintf("%032x", i+1), fmt.Sprintf("%032x", i+1), model.KeyID("s5"), start.Add(time.Duration(i)*time.Minute), true, nil, 0, 0, 10, 20)
		e.Tokens.Input = int64(i + 10)
		e.Tokens.Output = int64(i % 31)
		e.Tokens.CacheRead = int64(i % 9)
		e.Tokens.CacheCreation = int64(i % 7)
		e.Tokens.Total = e.Tokens.Input + e.Tokens.Output + int64(i%3)
		if i%4 == 0 {
			e.ServiceTierRequested = new("priority")
		}
		if i%5 == 0 {
			e.ServiceTierUsed = new("default")
		}
		if i%13 == 0 {
			e.Model = "alias-model"
			e.RequestedAlias = new("alias")
		}
		if i%17 == 0 {
			e.Provider = "kiro"
			e.ProviderCredits = new("0.123456789")
		}
		if i%19 == 0 {
			e.Model = "unknown"
		}
		if i%23 == 0 {
			e.Model = "slow"
		}
		if i == 1 {
			e.Tokens.Input = 2000000000000000
			e.Tokens.Total = e.Tokens.Input + e.Tokens.Output
		}
		if i == 2 {
			e.Tokens = model.TokenUsage{Schema: "normalized-v1", Quality: model.TokenQualityExact}
		}
		if i == 3 {
			e.Model = "precise"
			e.Tokens.Input = 123456789
			e.Tokens.Output = 1
			e.Tokens.CacheRead = 0
			e.Tokens.CacheCreation = 0
			e.Tokens.Total = e.Tokens.Input + e.Tokens.Output
		}
		events[i] = e
	}
	if err := s.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	// Legacy imports and missing snapshots must use the same scalar semantics.
	imported := events[1]
	imported.AttemptID = fmt.Sprintf("%032x", 999)
	imported.ProxyRequestID = imported.AttemptID
	if _, err := s.WriteImportBatch(ctx, []model.Event{imported}, "s5-import"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Conn().Exec(ctx, "ALTER TABLE "+s.table("analytics_events")+" UPDATE pricing_breakdown=NULL WHERE modulo(input_tokens,7)=0 SETTINGS mutations_sync=2"); err != nil {
		t.Fatal(err)
	}
	q := c2r2V2Query(model.OperationAnalysis, start, start.Add(24*time.Hour))
	check := func(label string) {
		t.Helper()
		want, werr := s.s5LegacyAnalysisCosts(ctx, q)
		got, gerr := s.analysisCosts(ctx, q)
		if fmt.Sprint(werr) != fmt.Sprint(gerr) || !reflect.DeepEqual(want, got) {
			t.Fatalf("%s\nold=%+v err=%v\nnew=%+v err=%v", label, want, werr, got, gerr)
		}
		analysis, err := s.Analysis(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		want.Meta.Partial = werr != nil
		if !reflect.DeepEqual(analysis.CostComponents, &want) {
			t.Fatalf("%s public analysis costs differ", label)
		}
	}
	check("mixed")
	changed := book
	changed.KiroUSDPerCredit = new(model.NanoUSD(1))
	changed.Rules = append([]aggregate.PricingRule(nil), book.Rules...)
	changed.Rules[0].InputPerMillion = &output
	if _, err := s.UpdatePriceBook(ctx, changed); err != nil {
		t.Fatal(err)
	}
	check("historical prices")
	if _, err := s.ApplyRetentionPolicy(ctx, start.Add(2*time.Hour), time.Time{}, 1000); err != nil {
		t.Fatal(err)
	}
	q.BucketWidth = "1h"
	check("hourly retained")
	if _, err := s.ApplyRetentionPolicy(ctx, start.Add(24*time.Hour), start.Add(24*time.Hour), 1000); err != nil {
		t.Fatal(err)
	}
	q.BucketWidth = "1d"
	check("daily retained")
}
