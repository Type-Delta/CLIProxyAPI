package aggregate

import (
	"math/big"
	"strings"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// RecordedPrice validates native export evidence without consulting the current
// pricebook. Intake must still be priced normally; only native imports use this.
func RecordedPrice(event model.Event) (PriceResult, bool) {
	validLabel := func(value string) bool {
		return strings.TrimSpace(value) != "" && len(value) <= model.MaxStoredStringBytes && utf8.ValidString(value)
	}
	if event.KnownCost == nil || *event.KnownCost < 0 || !validLabel(event.PriceRuleID) || !validLabel(event.PriceSource) {
		return PriceResult{}, false
	}
	result := PriceResult{KnownCost: event.KnownCost, UnpricedTokens: event.UnpricedTokens, RuleID: event.PriceRuleID, Source: event.PriceSource}
	if event.PriceSource == "kiro-credits" {
		if event.Provider != "kiro" || event.PricingBreakdown != nil || event.ProviderCredits == nil || !model.ValidProviderCredits(*event.ProviderCredits) || event.PriceUSDPerCredit == nil || *event.PriceUSDPerCredit < 0 || event.UnpricedTokens != 0 {
			return PriceResult{}, false
		}
		credits, _ := new(big.Rat).SetString(*event.ProviderCredits)
		expected, err := roundRat(credits.Mul(credits, new(big.Rat).SetInt64(int64(*event.PriceUSDPerCredit))))
		if err != nil || expected != *event.KnownCost {
			return PriceResult{}, false
		}
		result.PriceUSDPerCredit = event.PriceUSDPerCredit
		return result, true
	}
	snapshot := event.PricingBreakdown
	if snapshot == nil || event.PriceUSDPerCredit != nil || event.UnpricedTokens != event.Tokens.UnclassifiedTokens() || len(snapshot.ServiceTier) > model.MaxStoredStringBytes || !utf8.ValidString(snapshot.ServiceTier) {
		return PriceResult{}, false
	}
	for _, ratio := range []*string{snapshot.InputTierMultiplier, snapshot.OutputTierMultiplier} {
		if ratio != nil {
			if strings.TrimSpace(*ratio) == "" || strings.TrimSpace(*ratio) != *ratio || len(*ratio) > model.MaxStoredStringBytes {
				return PriceResult{}, false
			}
			if _, err := multiplier(*ratio); err != nil {
				return PriceResult{}, false
			}
		}
	}
	uncached := max(int64(0), event.Tokens.Input-event.Tokens.CacheRead-event.Tokens.CacheCreation)
	total := new(big.Rat)
	var previous model.NanoUSD
	for _, part := range []struct {
		component model.PricingComponent
		tokens    int64
	}{
		{snapshot.Input, uncached}, {snapshot.CacheRead, event.Tokens.CacheRead},
		{snapshot.CacheWrite, event.Tokens.CacheCreation}, {snapshot.Output, event.Tokens.Output},
	} {
		if part.component.Tokens != part.tokens || part.component.Cost < 0 || strings.TrimSpace(part.component.RatePerMillion) == "" || strings.TrimSpace(part.component.RatePerMillion) != part.component.RatePerMillion || len(part.component.RatePerMillion) > model.MaxStoredStringBytes {
			return PriceResult{}, false
		}
		rate, err := multiplier(part.component.RatePerMillion)
		if err != nil {
			return PriceResult{}, false
		}
		component := new(big.Rat).Mul(rate, new(big.Rat).SetInt64(part.tokens))
		// USD / million tokens to nano-USD / token is a factor of 1,000.
		component.Mul(component, big.NewRat(1000, 1))
		total.Add(total, component)
		rounded, err := roundRat(total)
		if err != nil || rounded-previous != part.component.Cost {
			return PriceResult{}, false
		}
		previous = rounded
	}
	if previous != *event.KnownCost {
		return PriceResult{}, false
	}
	result.Breakdown = snapshot
	return result, true
}
