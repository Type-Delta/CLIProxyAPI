package chstore

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

var (
	_ cpauk.PriceBookBackend            = (*Store)(nil)
	_ cpauk.PricingSnapshotBackend      = (*Store)(nil)
	_ cpauk.PricingRefreshBackend       = (*Store)(nil)
	_ cpauk.CatalogProvidersBackend     = (*Store)(nil)
	_ cpauk.CatalogBindingsReconfigurer = (*Store)(nil)
	_ cpauk.PricingMissingBackend       = (*Store)(nil)
	_ cpauk.ProviderCredentialBackend   = (*Store)(nil)
	_ store.KeyCatalogStore             = (*Store)(nil)
	_ store.ProviderQuotaStore          = (*Store)(nil)
	_ interface {
		RecordQuotaHistory(context.Context, []store.QuotaHistorySample, time.Time) error
		QuotaHistory(context.Context, string, time.Time, time.Time) ([]store.QuotaHistorySample, error)
	} = (*Store)(nil)
)
