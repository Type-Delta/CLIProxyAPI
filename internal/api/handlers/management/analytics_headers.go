package management

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/store"
)

// GetAnalyticsHeaders exposes retained, masked HTTP headers to full-access managers.
func (h *Handler) GetAnalyticsHeaders(c *gin.Context) {
	attemptID, hop := c.Param("attempt_id"), c.Query("hop")
	if !model.IsCorrelationID(attemptID) || (hop != "client_to_cpa" && hop != "cpa_to_client" && hop != "cpa_to_provider" && hop != "provider_to_cpa") {
		writeAnalyticsInvalid(c, fmt.Errorf("invalid attempt ID or header hop"))
		return
	}
	if c.Request.URL.Query().Has("cursor") || c.Request.URL.Query().Has("page_size") {
		writeAnalyticsInvalid(c, fmt.Errorf("pagination is not supported for headers"))
		return
	}
	values := c.Request.URL.Query()
	values.Del("hop")
	query, err := analyticsGETQuery(values, model.OperationEvents)
	if err != nil {
		writeAnalyticsInvalid(c, err)
		return
	}
	lookup, ok := h.analyticsService().(cpauk.EventLookup)
	if !ok {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	event, found, err := lookup.EventByAttemptID(c.Request.Context(), attemptID, query)
	if err != nil {
		writeAnalyticsError(c, classifyAnalyticsReadError(err))
		return
	}
	if !found {
		writeAnalyticsEnvelope(c, http.StatusNotFound, model.ErrorAnalyticsInvalidQuery, "The analytics event was not found.")
		return
	}
	handle := h.ClickHouse()
	if handle == nil || !handle.Configured() {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	db, err := handle.Get(c.Request.Context())
	if err != nil {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	detail, err := store.RequestHeaders(c.Request.Context(), db, event.ProxyRequestID, hop)
	if err != nil {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	setAnalyticsNoStore(c)
	c.JSON(http.StatusOK, detail)
}
