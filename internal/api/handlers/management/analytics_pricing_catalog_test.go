package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
)

type catalogProvidersHandlerService struct {
	*analyticsHandlerService
	refreshed bool
}

func (s *catalogProvidersHandlerService) RequestPricingRefresh() { s.refreshed = true }

func (s *catalogProvidersHandlerService) CatalogProviders(context.Context) ([]store.CatalogProvider, time.Time, error) {
	return []store.CatalogProvider{{ID: "zai", Name: "Z.AI"}}, time.Date(2026, 9, 11, 6, 0, 0, 0, time.UTC), nil
}

func TestGetAnalyticsPricingCatalogProviders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &catalogProvidersHandlerService{analyticsHandlerService: &analyticsHandlerService{state: model.StateReady}}
	handler := &Handler{analytics: service}
	request := httptest.NewRequest(http.MethodGet, "/v0/management/analytics/pricing/catalog-providers", nil)
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	handler.GetAnalyticsPricingCatalogProviders(context)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Providers        []store.CatalogProvider `json:"providers"`
		CatalogUpdatedAt *time.Time              `json:"catalog_updated_at"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !service.refreshed || len(body.Providers) != 1 || body.Providers[0].Name != "Z.AI" || body.CatalogUpdatedAt == nil {
		t.Fatalf("refreshed=%t body=%+v", service.refreshed, body)
	}
}
