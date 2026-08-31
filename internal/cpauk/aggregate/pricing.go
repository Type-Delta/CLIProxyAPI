package aggregate

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

// PricingRule prices an exact model or alias. Model matches take precedence.
// Prices are USD per million tokens represented as nano-USD values.
type PricingRule struct {
	ID                      string
	Model                   string
	Alias                   string
	InputPerMillion         *model.NanoUSD
	OutputPerMillion        *model.NanoUSD
	CacheReadMultiplier     string
	CacheCreationMultiplier string
	Source                  string
}

type PriceBook struct {
	Rules []PricingRule
}

type PriceResult struct {
	KnownCost      *model.NanoUSD
	UnpricedTokens int64
	RuleID         string
	Source         string
}

func (r PricingRule) Validate() error {
	if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.Source) == "" || len(r.ID) > model.MaxStoredStringBytes || len(r.Source) > model.MaxStoredStringBytes {
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
	return nil
}

func (p PriceBook) Price(event model.Event) (PriceResult, error) {
	rule := p.match(event.Model, event.RequestedAlias)
	if rule == nil || rule.InputPerMillion == nil || rule.OutputPerMillion == nil {
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
	total := new(big.Rat)
	addPricedTokens(total, uncachedInput, *rule.InputPerMillion, big.NewRat(1, 1))
	addPricedTokens(total, event.Tokens.CacheRead, *rule.InputPerMillion, readMultiplier)
	addPricedTokens(total, event.Tokens.CacheCreation, *rule.InputPerMillion, creationMultiplier)
	addPricedTokens(total, event.Tokens.Output, *rule.OutputPerMillion, big.NewRat(1, 1))
	cost, err := roundRat(total)
	if err != nil {
		return PriceResult{}, fmt.Errorf("pricing rule %s: %w", rule.ID, err)
	}
	return PriceResult{KnownCost: &cost, RuleID: rule.ID, Source: rule.Source}, nil
}

func (p PriceBook) match(eventModel string, alias *string) *PricingRule {
	for index := range p.Rules {
		if p.Rules[index].Model != "" && p.Rules[index].Model == eventModel {
			return &p.Rules[index]
		}
	}
	if alias == nil {
		return nil
	}
	for index := range p.Rules {
		if p.Rules[index].Alias != "" && p.Rules[index].Alias == *alias {
			return &p.Rules[index]
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
