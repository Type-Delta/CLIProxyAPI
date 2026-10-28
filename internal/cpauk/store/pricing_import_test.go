package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestNativeImportPreservesRecordedPricing(t *testing.T) {
	for _, creditPriced := range []bool{false, true} {
		t.Run(fmt.Sprintf("credits=%t", creditPriced), func(t *testing.T) {
			ctx := context.Background()
			input, output, credit := model.NanoUSD(1_000_000_000), model.NanoUSD(2_000_000_000), model.NanoUSD(20_000_000)
			rule := aggregate.PricingRule{ID: "original-rule", Model: "model-v2", Source: "original-source", InputPerMillion: &input, OutputPerMillion: &output}
			db, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: aggregate.PriceBook{Rules: []aggregate.PricingRule{rule}, KiroUSDPerCredit: &credit}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close(ctx) }()
			start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			event := v2Event(strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("a", 64), start.Add(time.Minute), true, nil, 0, 0, 100, 100)
			if creditPriced {
				event.Provider = "kiro"
				event.Model = "kiro-model"
				credits := "1.25"
				event.ProviderCredits = &credits
			}
			if err = db.WriteBatch(ctx, []model.Event{event}); err != nil {
				t.Fatal(err)
			}
			query := v2Query(model.OperationEvents, start, start.Add(time.Hour))
			page, err := db.Events(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			before := page.Events[0]
			encoded, err := json.Marshal(before)
			if err != nil {
				t.Fatal(err)
			}
			var replay model.Event
			if err = json.Unmarshal(encoded, &replay); err != nil {
				t.Fatal(err)
			}
			newInput, newOutput, newCredit := model.NanoUSD(10_000_000_000), model.NanoUSD(20_000_000_000), model.NanoUSD(100_000_000)
			rule.InputPerMillion = &newInput
			rule.OutputPerMillion = &newOutput
			rule.Source = "new-source"
			if _, err = db.UpdatePriceBook(ctx, aggregate.PriceBook{Rules: []aggregate.PricingRule{rule}, KiroUSDPerCredit: &newCredit}); err != nil {
				t.Fatal(err)
			}
			replay.AttemptID = strings.Repeat("3", 32)
			if count, err := db.WriteImportBatch(ctx, []model.Event{replay}, "native-roundtrip"); err != nil || count != 1 {
				t.Fatalf("import=%d %v", count, err)
			}
			after, found, err := db.EventByAttemptID(ctx, replay.AttemptID, query)
			if err != nil || !found {
				t.Fatalf("read=%v %v", found, err)
			}
			if !reflect.DeepEqual(before.PricingBreakdown, after.PricingBreakdown) || !reflect.DeepEqual(before.KnownCost, after.KnownCost) || !reflect.DeepEqual(before.PriceUSDPerCredit, after.PriceUSDPerCredit) || before.PriceRuleID != after.PriceRuleID || before.PriceSource != after.PriceSource || before.UnpricedTokens != after.UnpricedTokens {
				t.Fatalf("native import changed historical evidence: before=%+v after=%+v", before, after)
			}
			// The identical evidence remains untrusted on the ordinary intake path.
			replay.AttemptID = strings.Repeat("4", 32)
			if err = db.WriteBatch(ctx, []model.Event{replay}); err != nil {
				t.Fatal(err)
			}
			intake, _, err := db.EventByAttemptID(ctx, replay.AttemptID, query)
			if err != nil {
				t.Fatal(err)
			}
			if *intake.KnownCost == *before.KnownCost {
				t.Fatal("ordinary intake trusted imported pricing")
			}
			// Explicit maintenance is still allowed to replace the recorded evidence.
			if _, err = db.Reprice(ctx, RepriceOptions{Range: model.Range{Start: start, End: start.Add(time.Hour), TimeZone: "UTC"}}, nil); err != nil {
				t.Fatal(err)
			}
			repriced, _, err := db.EventByAttemptID(ctx, after.AttemptID, query)
			if err != nil {
				t.Fatal(err)
			}
			if *repriced.KnownCost != *intake.KnownCost || !reflect.DeepEqual(repriced.PricingBreakdown, intake.PricingBreakdown) || !reflect.DeepEqual(repriced.PriceUSDPerCredit, intake.PriceUSDPerCredit) {
				t.Fatal("maintenance did not replace imported evidence consistently")
			}
		})
	}
}

func TestNativeImportRepricesMissingOrInvalidSnapshot(t *testing.T) {
	ctx := context.Background()
	oldInput, oldOutput := model.NanoUSD(1_000_000_000), model.NanoUSD(2_000_000_000)
	oldBook := aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "old", Source: "old", Model: "model-v2", InputPerMillion: &oldInput, OutputPerMillion: &oldOutput}}}
	input, output := model.NanoUSD(10_000_000_000), model.NanoUSD(20_000_000_000)
	db, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "new", Source: "new", Model: "model-v2", InputPerMillion: &input, OutputPerMillion: &output}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(ctx) }()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for index, mutate := range []func(*model.Event){
		func(e *model.Event) { e.PricingBreakdown = nil },
		func(e *model.Event) { e.PricingBreakdown.Input.RatePerMillion = "not-a-rate" },
		func(e *model.Event) { e.PricingBreakdown.Input.Tokens++ },
		func(e *model.Event) { e.PricingBreakdown.Input.Cost++ },
		func(e *model.Event) { value := *e.KnownCost + 1; e.KnownCost = &value },
		func(e *model.Event) { e.PricingBreakdown.Input.RatePerMillion = " " },
		func(e *model.Event) { e.PricingBreakdown.Input.RatePerMillion = "-1" },
	} {
		event := v2Event(fmt.Sprintf("%032x", index+1), strings.Repeat("2", 32), strings.Repeat("a", 64), start.Add(time.Minute), true, nil, 0, 0, 100, 100)
		price, err := oldBook.Price(event)
		if err != nil {
			t.Fatal(err)
		}
		event.KnownCost = price.KnownCost
		event.PricingBreakdown = price.Breakdown
		event.PriceRuleID = price.RuleID
		event.PriceSource = price.Source
		mutate(&event)
		if _, err = db.WriteImportBatch(ctx, []model.Event{event}, "invalid-snapshot"); err != nil {
			t.Fatal(err)
		}
		after, _, err := db.EventByAttemptID(ctx, event.AttemptID, v2Query(model.OperationEvents, start, start.Add(time.Hour)))
		if err != nil {
			t.Fatal(err)
		}
		if after.PricingBreakdown.Input.RatePerMillion != "10" || after.PriceRuleID != "new" {
			t.Fatalf("case %d retained invalid pricing: %+v", index, after)
		}
	}
}
