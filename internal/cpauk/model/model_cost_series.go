package model

import "time"

// ModelCostSeries is an on-demand raw-event view. Partial marks unavailable
// retained history or prices that no longer match the stored event price.
type ModelCostSeries struct {
	Partial bool                    `json:"partial"`
	Meta    ResponseMeta            `json:"meta"`
	Model   string                  `json:"model"`
	Buckets []ModelCostSeriesBucket `json:"buckets"`
}

type ModelCostSeriesBucket struct {
	Start time.Time             `json:"start"`
	Tiers []ModelCostSeriesTier `json:"tiers"`
}

type ModelCostSeriesTier struct {
	Provider            string  `json:"provider"`
	ServiceTier         string  `json:"service_tier"`
	InputTokens         int64   `json:"input_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	TotalTokens         int64   `json:"total_tokens"`
	KnownCost           NanoUSD `json:"known_cost_usd"`
	// Rates and cost components are nullable when historical pricing cannot be reconstructed.
	Rates            []*string  `json:"rates_per_million"`
	Costs            []*NanoUSD `json:"cost_components"`
	InputMultiplier  *string    `json:"input_multiplier"`
	OutputMultiplier *string    `json:"output_multiplier"`
	CreditBasedCost  NanoUSD    `json:"credit_based_usd"`
}
