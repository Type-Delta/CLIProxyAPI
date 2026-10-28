package model

// PricingBreakdown is recorded by the pricing engine alongside KnownCost. Rates
// are USD per million tokens as exact rational strings. Component costs use
// cumulative rounding to nano-USD, so their sum equals the stored total.
// Credit pricing uses ProviderCredits and PriceUSDPerCredit instead.
type PricingBreakdown struct {
	Input                PricingComponent `json:"input"`
	Output               PricingComponent `json:"output"`
	CacheRead            PricingComponent `json:"cache_read"`
	CacheWrite           PricingComponent `json:"cache_write"`
	ServiceTier          string           `json:"service_tier,omitempty"`
	InputTierMultiplier  *string          `json:"input_tier_multiplier"`
	OutputTierMultiplier *string          `json:"output_tier_multiplier"`
}

type PricingComponent struct {
	Tokens         int64   `json:"tokens"`
	RatePerMillion string  `json:"rate_per_million_usd"`
	Cost           NanoUSD `json:"cost_usd"`
}
