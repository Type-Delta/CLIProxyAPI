package aggregate

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// PricingRule prices an exact provider/model or alias. Manual rules take
// precedence over discovered catalog rules. Prices are USD per million tokens
// represented as nano-USD values.
type PricingRule struct {
	// Provider is the exact CPA provider identifier for this rule. An empty
	// provider is retained for legacy management rules and matches any provider.
	Provider                string
	ID                      string
	Model                   string
	Alias                   string
	InputPerMillion         *model.NanoUSD
	OutputPerMillion        *model.NanoUSD
	CacheReadMultiplier     string
	CacheCreationMultiplier string
	ServiceTierPrices       map[string]ServiceTierPrice `json:",omitempty"`
	Source                  string
	// Catalog marks rules loaded from the managed remote catalog. It is kept
	// separate from Source so user supplied source labels cannot masquerade as
	// automatic rules.
	Catalog bool
}

// ServiceTierPrice holds complete token rates for one provider service tier.
// Cache multipliers are relative to this tier's input price.
type ServiceTierPrice struct {
	InputPerMillion         model.NanoUSD `json:"input_per_million_usd"`
	OutputPerMillion        model.NanoUSD `json:"output_per_million_usd"`
	CacheReadMultiplier     string        `json:"cache_read_multiplier,omitempty"`
	CacheCreationMultiplier string        `json:"cache_creation_multiplier,omitempty"`
}

type PriceBook struct {
	Rules            []PricingRule
	KiroUSDPerCredit *model.NanoUSD
}

func (p PriceBook) EffectiveKiroRate() model.NanoUSD {
	if p.KiroUSDPerCredit != nil {
		return *p.KiroUSDPerCredit
	}
	return 20_000_000
}

type PriceResult struct {
	Breakdown         *model.PricingBreakdown
	KnownCost         *model.NanoUSD
	UnpricedTokens    int64
	RuleID            string
	Source            string
	PriceUSDPerCredit *model.NanoUSD
}

func (r PricingRule) Validate() error {
	if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.Source) == "" || len(r.ID) > model.MaxStoredStringBytes || len(r.Source) > model.MaxStoredStringBytes || len(r.Provider) > model.MaxStoredStringBytes {
		return fmt.Errorf("pricing rule ID and source are required and bounded")
	}
	if (r.Model == "") == (r.Alias == "") || len(r.Model) > model.MaxStoredStringBytes || len(r.Alias) > model.MaxStoredStringBytes {
		return fmt.Errorf("pricing rule %s must select exactly one bounded model or alias", r.ID)
	}
	if (r.InputPerMillion == nil) != (r.OutputPerMillion == nil) {
		return fmt.Errorf("pricing rule %s prices must both be known or unknown", r.ID)
	}
	if r.InputPerMillion != nil && (*r.InputPerMillion < 0 || *r.OutputPerMillion < 0) {
		return fmt.Errorf("pricing rule %s contains a negative price", r.ID)
	}
	if _, err := multiplier(r.CacheReadMultiplier); err != nil {
		return fmt.Errorf("pricing rule %s cache-read multiplier: %w", r.ID, err)
	}
	if _, err := multiplier(r.CacheCreationMultiplier); err != nil {
		return fmt.Errorf("pricing rule %s cache-creation multiplier: %w", r.ID, err)
	}
	if len(r.ServiceTierPrices) > 32 {
		return fmt.Errorf("pricing rule %s has too many service tiers", r.ID)
	}
	for tier, price := range r.ServiceTierPrices {
		if tier == "" || tier != strings.ToLower(strings.TrimSpace(tier)) || len(tier) > model.MaxStoredStringBytes {
			return fmt.Errorf("pricing rule %s has an invalid service tier", r.ID)
		}
		if price.InputPerMillion < 0 || price.OutputPerMillion < 0 {
			return fmt.Errorf("pricing rule %s service tier %s contains a negative price", r.ID, tier)
		}
		if _, err := multiplier(price.CacheReadMultiplier); err != nil {
			return fmt.Errorf("pricing rule %s service tier %s cache-read multiplier: %w", r.ID, tier, err)
		}
		if _, err := multiplier(price.CacheCreationMultiplier); err != nil {
			return fmt.Errorf("pricing rule %s service tier %s cache-creation multiplier: %w", r.ID, tier, err)
		}
	}
	return nil
}

func (p PriceBook) Price(event model.Event) (PriceResult, error) {
	rule := p.match(event.Provider, event.Model, event.RequestedAlias)
	if (rule == nil || rule.Catalog) && event.Provider == "kiro" && event.ProviderCredits != nil {
		if !model.ValidProviderCredits(*event.ProviderCredits) {
			return PriceResult{}, fmt.Errorf("invalid provider credits")
		}
		rate := p.EffectiveKiroRate()
		if rate < 0 {
			return PriceResult{}, fmt.Errorf("negative credit rate")
		}
		credits, ok := new(big.Rat).SetString(*event.ProviderCredits)
		if !ok {
			return PriceResult{}, fmt.Errorf("invalid provider credits")
		}
		cost, err := roundRat(credits.Mul(credits, new(big.Rat).SetInt64(int64(rate))))
		if err != nil {
			return PriceResult{}, err
		}
		return PriceResult{KnownCost: &cost, RuleID: "kiro-usd-per-credit", Source: "kiro-credits", PriceUSDPerCredit: &rate}, nil
	}
	if rule == nil {
		return PriceResult{UnpricedTokens: event.Tokens.Total}, nil
	}
	baseInput, baseOutput := rule.InputPerMillion, rule.OutputPerMillion
	// A returned default tier is authoritative even when priority was requested.
	tier := ""
	if event.ServiceTierUsed != nil {
		tier = strings.TrimSpace(*event.ServiceTierUsed)
	}
	if tier == "" && event.ServiceTierRequested != nil {
		tier = strings.TrimSpace(*event.ServiceTierRequested)
	}
	if price, ok := rule.ServiceTierPrices[strings.ToLower(tier)]; ok {
		selected := *rule
		selected.InputPerMillion, selected.OutputPerMillion = &price.InputPerMillion, &price.OutputPerMillion
		selected.CacheReadMultiplier, selected.CacheCreationMultiplier = price.CacheReadMultiplier, price.CacheCreationMultiplier
		rule = &selected
	}
	if rule.InputPerMillion == nil || rule.OutputPerMillion == nil {
		return PriceResult{UnpricedTokens: event.Tokens.Total}, nil
	}
	if *rule.InputPerMillion < 0 || *rule.OutputPerMillion < 0 {
		return PriceResult{}, fmt.Errorf("pricing rule %s contains a negative price", rule.ID)
	}
	readMultiplier, err := multiplier(rule.CacheReadMultiplier)
	if err != nil {
		return PriceResult{}, fmt.Errorf("pricing rule %s cache-read multiplier: %w", rule.ID, err)
	}
	creationMultiplier, err := multiplier(rule.CacheCreationMultiplier)
	if err != nil {
		return PriceResult{}, fmt.Errorf("pricing rule %s cache-creation multiplier: %w", rule.ID, err)
	}

	uncachedInput := event.Tokens.Input - event.Tokens.CacheRead - event.Tokens.CacheCreation
	if uncachedInput < 0 {
		uncachedInput = 0
	}
	breakdown := &model.PricingBreakdown{ServiceTier: tier}
	ratio := func(applied, base *model.NanoUSD) *string {
		if base == nil || *base == 0 {
			return nil
		}
		value := new(big.Rat).SetFrac64(int64(*applied), int64(*base)).RatString()
		return &value
	}
	breakdown.InputTierMultiplier = ratio(rule.InputPerMillion, baseInput)
	breakdown.OutputTierMultiplier = ratio(rule.OutputPerMillion, baseOutput)
	total := new(big.Rat)
	var previous model.NanoUSD
	for _, part := range []struct {
		target     *model.PricingComponent
		tokens     int64
		rate       model.NanoUSD
		multiplier *big.Rat
	}{
		{&breakdown.Input, uncachedInput, *rule.InputPerMillion, big.NewRat(1, 1)},
		{&breakdown.CacheRead, event.Tokens.CacheRead, *rule.InputPerMillion, readMultiplier},
		{&breakdown.CacheWrite, event.Tokens.CacheCreation, *rule.InputPerMillion, creationMultiplier},
		{&breakdown.Output, event.Tokens.Output, *rule.OutputPerMillion, big.NewRat(1, 1)},
	} {
		addPricedTokens(total, part.tokens, part.rate, part.multiplier)
		rounded, err := roundRat(total)
		if err != nil {
			return PriceResult{}, err
		}
		rate := new(big.Rat).Mul(new(big.Rat).SetFrac64(int64(part.rate), 1_000_000_000), part.multiplier)
		// Emit terminating decimals exactly; retain fractions for nonterminating rates.
		denominator := new(big.Int).Set(rate.Denom())
		digits := 0
		for _, factor := range []int64{2, 5} {
			count := 0
			for new(big.Int).Mod(denominator, big.NewInt(factor)).Sign() == 0 {
				denominator.Quo(denominator, big.NewInt(factor))
				count++
			}
			digits = max(digits, count)
		}
		rateText := rate.RatString()
		if denominator.Cmp(big.NewInt(1)) == 0 {
			rateText = rate.FloatString(digits)
		}
		*part.target = model.PricingComponent{Tokens: part.tokens, RatePerMillion: rateText, Cost: rounded - previous}
		previous = rounded
	}

	cost, err := roundRat(total)
	if err != nil {
		return PriceResult{}, fmt.Errorf("pricing rule %s: %w", rule.ID, err)
	}
	return PriceResult{
		Breakdown:      breakdown,
		KnownCost:      &cost,
		UnpricedTokens: event.Tokens.UnclassifiedTokens(),
		RuleID:         rule.ID,
		Source:         rule.Source,
	}, nil
}

func (p PriceBook) match(eventProvider, eventModel string, alias *string) *PricingRule {
	// Explicit management rules have precedence over discovered model rules.
	// In particular, a manual alias is allowed to override an automatic model
	// rule because aliases are the operator's explicit billing decision.
	for _, catalog := range []bool{false, true} {
		for _, aliasMatch := range []bool{false, true} {
			for _, providerSpecific := range []bool{true, false} {
				for index := range p.Rules {
					rule := &p.Rules[index]
					if rule.Catalog != catalog || (providerSpecific && rule.Provider != eventProvider) || (!providerSpecific && rule.Provider != "") {
						continue
					}
					if aliasMatch {
						if alias == nil || rule.Alias == "" || rule.Alias != *alias {
							continue
						}
					} else if rule.Model == "" || rule.Model != eventModel {
						continue
					}
					return rule
				}
			}
		}
	}
	return nil
}

func multiplier(value string) (*big.Rat, error) {
	if strings.TrimSpace(value) == "" {
		return big.NewRat(1, 1), nil
	}
	parsed, ok := new(big.Rat).SetString(value)
	if !ok || parsed.Sign() < 0 {
		return nil, fmt.Errorf("invalid nonnegative decimal %q", value)
	}
	return parsed, nil
}

func addPricedTokens(total *big.Rat, tokens int64, price model.NanoUSD, multiplier *big.Rat) {
	component := new(big.Rat).SetInt64(tokens)
	component.Mul(component, new(big.Rat).SetInt64(int64(price)))
	component.Mul(component, multiplier)
	component.Quo(component, big.NewRat(1_000_000, 1))
	total.Add(total, component)
}

func roundRat(value *big.Rat) (model.NanoUSD, error) {
	numerator := new(big.Int).Set(value.Num())
	denominator := new(big.Int).Set(value.Denom())
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	if new(big.Int).Lsh(new(big.Int).Abs(remainder), 1).Cmp(denominator) >= 0 {
		if numerator.Sign() < 0 {
			quotient.Sub(quotient, big.NewInt(1))
		} else {
			quotient.Add(quotient, big.NewInt(1))
		}
	}
	if !quotient.IsInt64() {
		return 0, fmt.Errorf("cost_overflow")
	}
	return model.NanoUSD(quotient.Int64()), nil
}
