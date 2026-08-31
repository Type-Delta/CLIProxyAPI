package management

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func (h *Handler) GetAnalyticsEvent(c *gin.Context) {
	attemptID := c.Param("attempt_id")
	if !model.IsCorrelationID(attemptID) {
		writeAnalyticsInvalid(c, fmt.Errorf("invalid attempt ID"))
		return
	}
	if c.Request.URL.Query().Has("cursor") || c.Request.URL.Query().Has("page_size") {
		writeAnalyticsInvalid(c, fmt.Errorf("pagination is not supported for event detail"))
		return
	}
	query, err := analyticsGETQuery(c.Request.URL.Query(), model.OperationEvents)
	if err != nil {
		writeAnalyticsInvalid(c, err)
		return
	}
	service := h.analyticsService()
	if service == nil {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	lookup, ok := service.(cpauk.EventLookup)
	if !ok {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	event, found, err := lookup.EventByAttemptID(c.Request.Context(), attemptID, query)
	if err != nil {
		writeAnalyticsError(c, classifyAnalyticsReadError(err))
		return
	}
	if found {
		setAnalyticsNoStore(c)
		c.JSON(http.StatusOK, event)
		return
	}
	writeAnalyticsEnvelope(c, http.StatusNotFound, model.ErrorAnalyticsInvalidQuery, "The analytics event was not found.")
}

type analyticsExportRequest struct {
	Query   model.Query `json:"query"`
	MaxRows int         `json:"max_rows,omitempty"`
}

func (h *Handler) CreateAnalyticsExport(c *gin.Context) {
	var request analyticsExportRequest
	if err := decodeAnalyticsJSON(c, &request, model.MaxQueryBodyBytes); err != nil {
		writeAnalyticsInvalid(c, err)
		return
	}
	if err := request.Query.Validate(); err != nil || request.Query.Operation != model.OperationEvents {
		writeAnalyticsInvalid(c, err)
		return
	}
	if request.MaxRows == 0 {
		request.MaxRows = model.MaxExportRows
	}
	if request.MaxRows < 1 || request.MaxRows > model.MaxExportRows {
		writeAnalyticsEnvelope(c, http.StatusRequestEntityTooLarge, model.ErrorAnalyticsExportTooLarge, "The analytics export exceeds the row limit.")
		return
	}
	service := h.analyticsService()
	if service == nil {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	var output bytes.Buffer
	rows, err := writeAnalyticsEventsCSV(c.Request.Context(), &output, service.Reader(), request.Query, request.MaxRows)
	if err != nil {
		var tooLarge modelExportTooLargeError
		if errors.As(err, &tooLarge) {
			writeAnalyticsEnvelope(c, http.StatusRequestEntityTooLarge, model.ErrorAnalyticsExportTooLarge, "The analytics export exceeds the row limit.")
			return
		}
		writeAnalyticsError(c, classifyAnalyticsReadError(err))
		return
	}
	setAnalyticsNoStore(c)
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="analytics-events.csv"`)
	c.Header("X-Analytics-Export-Rows", strconv.Itoa(rows))
	c.Data(http.StatusOK, "text/csv; charset=utf-8", output.Bytes())
}
