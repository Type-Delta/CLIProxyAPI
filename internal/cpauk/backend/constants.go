package backend

import "time"

const (
	MaxPricingRules           = 10_000
	MaxProviderQuotaSnapshots = 10_000
	MaxKeyLifecycleRows       = 10_000
	ModelsDevSource           = "models.dev"
	PricingCatalogTTL         = 6 * time.Hour
	PricingRetrySuppression   = 15 * time.Minute
)
