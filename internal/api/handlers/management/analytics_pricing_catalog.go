package management

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
)

type catalogProvidersResponse struct {
	Providers        []catalogProviderResponse `json:"providers"`
	CatalogUpdatedAt *time.Time                `json:"catalog_updated_at"`
}

type catalogProviderResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type analyticsCatalogProviders interface {
	CatalogProviders(context.Context) ([]store.CatalogProvider, time.Time, error)
}

func (h *Handler) GetAnalyticsPricingCatalogProviders(c *gin.Context) {
	service, err := h.analyticsServiceForRead()
	if err != nil {
		writeAnalyticsError(c, err)
		return
	}
	if requester, ok := service.(analyticsPricingRefreshRequester); ok {
		requester.RequestPricingRefresh()
	}
	providerStore, ok := service.(analyticsCatalogProviders)
	if !ok {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	providers, updatedAt, err := providerStore.CatalogProviders(c.Request.Context())
	if err != nil {
		writeAnalyticsError(c, classifyAnalyticsReadError(err))
		return
	}
	response := catalogProvidersResponse{Providers: make([]catalogProviderResponse, 0, len(providers))}
	for _, provider := range providers {
		response.Providers = append(response.Providers, catalogProviderResponse{ID: provider.ID, Name: provider.Name})
	}
	if !updatedAt.IsZero() {
		updated := updatedAt.UTC()
		response.CatalogUpdatedAt = &updated
	}
	setAnalyticsNoStore(c)
	c.JSON(http.StatusOK, response)
}
