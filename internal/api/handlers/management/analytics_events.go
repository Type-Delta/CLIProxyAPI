package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	Format  string      `json:"format,omitempty"`
}

func (h *Handler) CreateAnalyticsExport(c *gin.Context) {
	var request analyticsExportRequest
	if err := decodeAnalyticsJSON(c, &request, model.MaxQueryBodyBytes); err != nil {
		writeAnalyticsInvalid(c, err)
		return
	}
	if request.Query.Cursor != "" {
		writeAnalyticsInvalid(c, fmt.Errorf("exports do not accept cursors"))
		return
	}
	if request.Query.Range != nil {
		if err := resolveAnalyticsRange(&request.Query, time.Now().UTC()); err != nil {
			writeAnalyticsInvalid(c, err)
			return
		}
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
	format := strings.ToLower(strings.TrimSpace(request.Format))
	if format == "" {
		format = "csv"
	}
	var rows int
	var err error
	switch format {
	case "csv":
		rows, err = writeAnalyticsEventsCSV(c.Request.Context(), &output, service.Reader(), request.Query, request.MaxRows)
	case "json":
		rows, err = writeAnalyticsEventsJSON(c.Request.Context(), &output, service.Reader(), request.Query, request.MaxRows)
	default:
		writeAnalyticsInvalid(c, fmt.Errorf("unsupported export format %q", request.Format))
		return
	}
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
	contentType, filename := "text/csv; charset=utf-8", "analytics-events.csv"
	if format == "json" {
		contentType, filename = "application/json; charset=utf-8", "analytics-events.json"
	}
	c.Header("Content-Type", contentType)
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Header("X-Analytics-Export-Rows", strconv.Itoa(rows))
	c.Data(http.StatusOK, contentType, output.Bytes())
}

var analyticsEventExportColumns = []string{
	"schema_version", "attempt_id", "proxy_request_id", "request_id_quality", "key_id", "requested_at", "provider", "executor_type", "model", "requested_alias", "endpoint_class", "auth_type", "credential_id", "credential_id_algorithm", "succeeded", "upstream_status_code", "error_class", "latency_ms", "time_to_first_token_ms", "service_tier_requested", "service_tier_used", "generated", "input_tokens", "output_tokens", "reasoning_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens", "total_tokens", "accounting_schema", "token_quality", "known_cost_usd", "unpriced_tokens", "price_rule_id", "price_source", "import_batch_id", "source",
}

func analyticsEventExportValues(event model.Event) map[string]any {
	values := map[string]any{
		"schema_version": event.SchemaVersion, "attempt_id": event.AttemptID, "proxy_request_id": event.ProxyRequestID, "request_id_quality": event.RequestIDQuality,
		"key_id": event.KeyID, "requested_at": event.RequestedAt, "provider": event.Provider, "executor_type": event.ExecutorType,
		"model": event.Model, "requested_alias": event.RequestedAlias, "endpoint_class": event.EndpointClass, "auth_type": event.AuthType,
		"credential_id": event.CredentialID, "credential_id_algorithm": event.CredentialIDAlgorithm, "succeeded": event.Succeeded,
		"upstream_status_code": event.UpstreamStatusCode, "error_class": event.ErrorClass, "latency_ms": event.LatencyMS,
		"time_to_first_token_ms": event.TimeToFirstTokenMS, "service_tier_requested": event.ServiceTierRequested, "service_tier_used": event.ServiceTierUsed,
		"generated": event.Generated, "input_tokens": event.Tokens.Input, "output_tokens": event.Tokens.Output, "reasoning_tokens": event.Tokens.Reasoning,
		"cached_tokens": event.Tokens.Cached, "cache_read_tokens": event.Tokens.CacheRead, "cache_creation_tokens": event.Tokens.CacheCreation,
		"total_tokens": event.Tokens.Total, "accounting_schema": event.Tokens.Schema, "token_quality": event.Tokens.Quality,
		"known_cost_usd": event.KnownCost, "unpriced_tokens": event.UnpricedTokens,
	}
	values["price_rule_id"], values["price_source"], values["import_batch_id"], values["source"] = event.PriceRuleID, event.PriceSource, event.ImportBatchID, event.Source
	return values
}

func writeAnalyticsEventsJSON(ctx context.Context, writer io.Writer, reader cpauk.Reader, query model.Query, maxRows int) (int, error) {
	if query.Operation != model.OperationEvents || maxRows < 1 || maxRows > model.MaxExportRows {
		return 0, fmt.Errorf("invalid export query")
	}
	if _, err := io.WriteString(writer, "["); err != nil {
		return 0, err
	}
	encoder := json.NewEncoder(writer)
	written := 0
	first := true
	previousCursor := query.Cursor
	for {
		remaining := maxRows - written
		if remaining <= 0 {
			return written, modelExportTooLargeError{}
		}
		query.PageSize = min(remaining, model.MaxPageSize)
		page, err := reader.Events(ctx, query)
		if err != nil {
			return written, err
		}
		for _, event := range page.Events {
			if !first {
				if _, err := io.WriteString(writer, ","); err != nil {
					return written, err
				}
			}
			first = false
			if err := encoder.Encode(analyticsEventExportValues(event)); err != nil {
				return written, err
			}
			written++
		}
		if page.Meta.NextCursor == "" {
			break
		}
		if len(page.Events) == 0 || page.Meta.NextCursor == previousCursor {
			return written, cpauk.ErrInternal
		}
		query.Cursor, previousCursor = page.Meta.NextCursor, page.Meta.NextCursor
	}
	if _, err := io.WriteString(writer, "]"); err != nil {
		return written, err
	}
	return written, nil
}
