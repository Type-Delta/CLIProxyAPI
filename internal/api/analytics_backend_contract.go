package api

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/chstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/collector"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/importer"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/maintenance"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

var (
	_ cpauk.Backend                     = (*chstore.Store)(nil)
	_ cpauk.ReaderV2                    = (*chstore.Store)(nil)
	_ cpauk.EventLookup                 = (*chstore.Store)(nil)
	_ cpauk.PriceBookBackend            = (*chstore.Store)(nil)
	_ cpauk.PricingSnapshotBackend      = (*chstore.Store)(nil)
	_ cpauk.PricingRefreshBackend       = (*chstore.Store)(nil)
	_ cpauk.CatalogProvidersBackend     = (*chstore.Store)(nil)
	_ cpauk.CatalogBindingsReconfigurer = (*chstore.Store)(nil)
	_ cpauk.BackendReconfigurer         = (*chstore.Store)(nil)
	_ cpauk.StorageBudgetReconfigurer   = (*chstore.Store)(nil)
	_ cpauk.PricingMissingBackend       = (*chstore.Store)(nil)
	_ cpauk.ProviderCredentialBackend   = (*chstore.Store)(nil)
	_ backend.KeyCatalogStore           = (*chstore.Store)(nil)
	_ backend.ProviderQuotaStore        = (*chstore.Store)(nil)
	_ collector.PatchWriter             = (*chstore.Store)(nil)
	_ importer.Destination              = (*chstore.Store)(nil)
	_ maintenance.Store                 = (*chstore.Store)(nil)
	_ interface {
		ModelCostSeries(context.Context, model.Query) (model.ModelCostSeries, error)
	} = (*chstore.Store)(nil)
	_ interface {
		RecordQuotaHistory(context.Context, []backend.QuotaHistorySample, time.Time) error
		QuotaHistory(context.Context, string, time.Time, time.Time) ([]backend.QuotaHistorySample, error)
	} = (*chstore.Store)(nil)
)
