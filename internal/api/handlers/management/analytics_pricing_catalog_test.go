package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

func TestAnalyticsPricingServiceTierRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &analyticsPricingTestService{analyticsHandlerService: &analyticsHandlerService{reader: &analyticsHandlerReader{}, state: model.StateReady}}
	handler := &Handler{analytics: service}
	body := `{"rules":[{"rule_id":"tier-rule","match":{"model":"model"},"input_per_million_usd":"1","output_per_million_usd":"2","service_tier_prices":{"priority":{"input_per_million_usd":"2.5","output_per_million_usd":"5","cache_read_multiplier":"0.1"}},"source":"admin"}]}`
	ctx, response := v2Request(http.MethodPut, "/v8/management/analytics/pricing", body)
	handler.PutAnalyticsPricing(ctx)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"service_tier_prices":{"priority":`) {
		t.Fatalf("PUT status=%d body=%s", response.Code, response.Body.String())
	}
	ctx, response = v2Request(http.MethodGet, "/v8/management/analytics/pricing", "")
	handler.GetAnalyticsPricing(ctx)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"input_per_million_usd":"2.5"`) {
		t.Fatalf("GET status=%d body=%s", response.Code, response.Body.String())
	}
	priority := "priority"
	priced, err := service.book.Price(model.Event{Model: "model", ServiceTierUsed: &priority, Tokens: model.TokenUsage{Input: 100, Output: 100, Total: 200}})
	if err != nil || priced.KnownCost == nil || priced.KnownCost.String() != "0.00075" {
		t.Fatalf("price=%+v err=%v", priced, err)
	}
}

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
