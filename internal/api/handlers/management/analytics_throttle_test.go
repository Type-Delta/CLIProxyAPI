package management

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func TestAnalyticsAdminExpensiveRoutesAreThrottled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{analyticsAdminLimiter: newAnalyticsAdminRateLimiter()}
	engine := gin.New()
	group := engine.Group("/v0/management/analytics")
	group.Use(handler.AnalyticsRateLimitMiddleware())
	group.POST("/exports", func(c *gin.Context) { c.Status(http.StatusAccepted) })

	for attempt := 1; attempt <= 21; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/v0/management/analytics/exports", strings.NewReader("{}"))
		request.RemoteAddr = "192.0.2.8:41321"
		request.Header.Set("X-Forwarded-For", "198.51.100."+strconv.Itoa(attempt))
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		if attempt <= 20 && response.Code != http.StatusAccepted {
			t.Fatalf("attempt %d status=%d", attempt, response.Code)
		}
		if attempt == 21 && (response.Code != http.StatusTooManyRequests || !strings.Contains(response.Body.String(), string(model.ErrorAnalyticsThrottled))) {
			t.Fatalf("throttled response status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

// One analytics page mount fans out into several card loads; the query budget must
// leave room for repeated visits within a minute before it throttles.
func TestAnalyticsQueryBudgetAllowsRepeatedPageLoads(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{analyticsAdminLimiter: newAnalyticsAdminRateLimiter()}
	engine := gin.New()
	group := engine.Group("/v0/management/analytics")
	group.Use(handler.AnalyticsRateLimitMiddleware())
	group.POST("/query", func(c *gin.Context) { c.Status(http.StatusOK) })

	const queryBudget = 600
	for attempt := 1; attempt <= queryBudget+1; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/v0/management/analytics/query", strings.NewReader("{}"))
		request.RemoteAddr = "192.0.2.9:41321"
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		if attempt <= queryBudget && response.Code != http.StatusOK {
			t.Fatalf("attempt %d status=%d", attempt, response.Code)
		}
		if attempt == queryBudget+1 {
			if response.Code != http.StatusTooManyRequests {
				t.Fatalf("attempt %d status=%d, want 429", attempt, response.Code)
			}
			if got := response.Header().Get("Retry-After"); got != "60" {
				t.Fatalf("Retry-After=%q, want 60", got)
			}
		}
	}
}
