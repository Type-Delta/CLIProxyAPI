package management

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/maintenance"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func (h *Handler) GetAnalyticsSummary(c *gin.Context) {
	h.runAnalyticsGET(c, model.OperationSummary)
}

func (h *Handler) GetAnalyticsTimeseries(c *gin.Context) {
	h.runAnalyticsGET(c, model.OperationTimeseries)
}

func (h *Handler) GetAnalyticsDimensions(c *gin.Context) {
	h.runAnalyticsGET(c, model.OperationDimensions)
}

func (h *Handler) GetAnalyticsEvents(c *gin.Context) {
	h.runAnalyticsGET(c, model.OperationEvents)
}

func (h *Handler) GetAnalyticsLeaderboard(c *gin.Context) {
	h.runAnalyticsGET(c, model.OperationLeaderboard)
}

func (h *Handler) PostAnalyticsQuery(c *gin.Context) {
	data, err := readBoundedBody(c, model.MaxQueryBodyBytes)
	if err != nil {
		writeAnalyticsInvalid(c, err)
		return
	}
	query, err := parseAnalyticsBodyQuery(data)
	if err != nil {
		writeAnalyticsInvalid(c, err)
		return
	}
	h.executeAnalyticsQuery(c, query)
}

func (h *Handler) runAnalyticsGET(c *gin.Context, operation model.Operation) {
	query, err := analyticsGETQuery(c.Request.URL.Query(), operation)
	if err != nil {
		writeAnalyticsInvalid(c, err)
		return
	}
	h.executeAnalyticsQuery(c, query)
}

func (h *Handler) executeAnalyticsQuery(c *gin.Context, query model.Query) {
	service := h.analyticsService()
	if service == nil {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	var result any
	var err error
	reader := service.Reader()
	switch query.Operation {
	case model.OperationSummary:
		result, err = reader.Summary(c.Request.Context(), query)
	case model.OperationTimeseries:
		result, err = reader.Timeseries(c.Request.Context(), query)
	case model.OperationDimensions:
		result, err = reader.Dimensions(c.Request.Context(), query)
	case model.OperationEvents:
		result, err = reader.Events(c.Request.Context(), query)
	case model.OperationLeaderboard:
		result, err = reader.Leaderboard(c.Request.Context(), query)
	case model.OperationActivity:
		v2, ok := reader.(interface {
			Activity(context.Context, model.Query) (model.Activity, error)
		})
		if !ok {
			writeAnalyticsError(c, cpauk.ErrUnavailable)
			return
		}
		result, err = v2.Activity(c.Request.Context(), query)
	case model.OperationAnalysis:
		v2, ok := reader.(interface {
			Analysis(context.Context, model.Query) (model.Analysis, error)
		})
		if !ok {
			writeAnalyticsError(c, cpauk.ErrUnavailable)
			return
		}
		result, err = v2.Analysis(c.Request.Context(), query)
	default:
		writeAnalyticsInvalid(c, fmt.Errorf("unsupported operation"))
		return
	}
	if err != nil {
		writeAnalyticsError(c, classifyAnalyticsReadError(err))
		return
	}
	result = analyticsResultWithRange(result, query)
	setAnalyticsNoStore(c)
	c.JSON(http.StatusOK, result)
}

func analyticsResultWithRange(result any, query model.Query) any {
	resolved := model.Range{Start: query.Start.UTC(), End: query.End.UTC(), TimeZone: query.TimeZone}
	switch value := result.(type) {
	case model.Summary:
		value.Meta.Range = resolved
		return value
	case model.Timeseries:
		value.Meta.Range = resolved
		return value
	case model.DimensionPage:
		value.Meta.Range = resolved
		return value
	case model.EventPage:
		value.Meta.Range = resolved
		return value
	case model.LeaderboardPage:
		value.Meta.Range = resolved
		return value
	case model.Activity:
		value.Meta.Range = resolved
		return value
	case model.Analysis:
		value.Meta.Range = resolved
		return value
	default:
		return result
	}
}

func (h *Handler) analyticsService() cpauk.Service {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.analytics
}

func (h *Handler) analyticsServiceForRead() (cpauk.Service, error) {
	service := h.analyticsService()
	if service == nil {
		return nil, cpauk.ErrUnavailable
	}
	capabilities := service.Capabilities()
	if !capabilities.Enabled || capabilities.State == model.StateDisabled {
		return nil, cpauk.ErrDisabled
	}
	return service, nil
}

func analyticsGETQuery(values url.Values, operation model.Operation) (model.Query, error) {
	allowed := map[string]struct{}{
		"start": {}, "end": {}, "time_zone": {}, "cursor": {}, "page_size": {},
		"bucket_width": {}, "dimension": {}, "sort_by": {}, "provider": {}, "model": {},
		"endpoint_class": {}, "auth_type": {}, "service_tier": {}, "success": {},
		"error_class": {}, "status_code": {}, "token_quality": {}, "generated": {}, "result": {}, "source": {},
	}
	for name := range values {
		if name == "key_id" || name == "key_ids" {
			return model.Query{}, fmt.Errorf("key IDs are forbidden in query strings")
		}
		if _, ok := allowed[name]; !ok {
			return model.Query{}, fmt.Errorf("unknown query field %q", name)
		}
	}
	for _, name := range []string{"start", "end", "time_zone", "cursor", "page_size", "bucket_width", "dimension", "sort_by", "success", "generated"} {
		if len(values[name]) > 1 {
			return model.Query{}, fmt.Errorf("query field %q may appear only once", name)
		}
	}
	start, err := time.Parse(time.RFC3339Nano, values.Get("start"))
	if err != nil {
		return model.Query{}, fmt.Errorf("start must be an RFC 3339 UTC timestamp")
	}
	end, err := time.Parse(time.RFC3339Nano, values.Get("end"))
	if err != nil {
		return model.Query{}, fmt.Errorf("end must be an RFC 3339 UTC timestamp")
	}
	query := model.Query{
		SchemaVersion: model.QuerySchemaVersion, Operation: operation, Start: start,
		End: end, TimeZone: values.Get("time_zone"), Cursor: values.Get("cursor"),
		BucketWidth: values.Get("bucket_width"), Dimension: values.Get("dimension"),
		SortBy: model.LeaderboardSort(values.Get("sort_by")), Filters: make(map[string]json.RawMessage),
	}
	if raw := values.Get("page_size"); raw != "" {
		query.PageSize, err = strconv.Atoi(raw)
		if err != nil {
			return model.Query{}, fmt.Errorf("page_size must be an integer")
		}
	}
	for _, name := range []string{"provider", "model", "endpoint_class", "auth_type", "service_tier", "error_class", "token_quality", "source"} {
		if entries, ok := values[name]; ok {
			encoded, errEncode := json.Marshal(entries)
			if errEncode != nil {
				return model.Query{}, errEncode
			}
			query.Filters[name] = encoded
		}
	}
	if raw := values.Get("result"); raw != "" {
		query.Filters["result"], _ = json.Marshal(raw)
	}
	for _, name := range []string{"success", "generated"} {
		if raw := values.Get(name); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse != nil {
				return model.Query{}, fmt.Errorf("%s must be a boolean", name)
			}
			query.Filters[name], _ = json.Marshal(parsed)
		}
	}
	if entries, ok := values["status_code"]; ok {
		statuses := make([]int, 0, len(entries))
		for _, raw := range entries {
			status, errStatus := strconv.Atoi(raw)
			if errStatus != nil {
				return model.Query{}, fmt.Errorf("status_code must be an integer")
			}
			statuses = append(statuses, status)
		}
		query.Filters["status_code"], _ = json.Marshal(statuses)
	}
	if len(query.Filters) == 0 {
		query.Filters = nil
	}
	if err = query.Validate(); err != nil {
		return model.Query{}, err
	}
	if query.Cursor != "" && !query.CursorAllowedInGET() {
		return model.Query{}, fmt.Errorf("identity-bearing cursor is forbidden in GET queries")
	}
	return query, nil
}

// ParseViewerAnalyticsQuery applies the viewer's fixed key scope after it has
// rejected any key identity supplied by the client.
func ParseViewerAnalyticsQuery(values url.Values, operation model.Operation, keyID string) (model.Query, error) {
	query, err := analyticsGETQuery(values, operation)
	if err != nil {
		return model.Query{}, err
	}
	if !model.IsFullKeyID(keyID) {
		return model.Query{}, fmt.Errorf("invalid viewer scope")
	}
	query.KeyIDs = []string{keyID}
	if err = query.Validate(); err != nil {
		return model.Query{}, err
	}
	return query, nil
}

func parseAnalyticsBodyQuery(data []byte) (model.Query, error) {
	if len(data) == 0 || len(data) > model.MaxQueryBodyBytes {
		return model.Query{}, fmt.Errorf("query body exceeds its bounds")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var query model.Query
	if err := decoder.Decode(&query); err != nil {
		return model.Query{}, fmt.Errorf("decode query v1: %w", err)
	}
	if err := requireAnalyticsJSONEOF(decoder); err != nil {
		return model.Query{}, err
	}
	if query.Range != nil {
		if err := resolveAnalyticsRange(&query, time.Now().UTC()); err != nil {
			return model.Query{}, err
		}
	}
	if err := query.Validate(); err != nil {
		return model.Query{}, err
	}
	parsed, err := model.ParseQuery(data)
	if err == nil {
		if parsed.Range != nil {
			if err := resolveAnalyticsRange(&parsed, time.Now().UTC()); err != nil {
				return model.Query{}, err
			}
		}
		return parsed, nil
	}
	if query.Cursor == "" || err.Error() != "cursor query requires a cursor codec" {
		return model.Query{}, err
	}
	return query, nil
}

// resolveAnalyticsRange converts the v2 named range into the normalized query
// bounds consumed by v1-compatible readers. The original named range remains
// attached so adapters can echo the request form if desired.
func resolveAnalyticsRange(query *model.Query, now time.Time) error {
	if query == nil || query.Range == nil {
		return nil
	}
	if query.SchemaVersion != model.QuerySchemaVersionV2 {
		return fmt.Errorf("named ranges require query schema version %d", model.QuerySchemaVersionV2)
	}
	rangeRequest := query.Range
	if err := rangeRequest.Validate(); err != nil {
		return err
	}
	location, err := time.LoadLocation(rangeRequest.TimeZone)
	if err != nil {
		return fmt.Errorf("invalid IANA range time zone %q", rangeRequest.TimeZone)
	}
	localNow := now.In(location)
	var start, end time.Time
	switch rangeRequest.Preset {
	case "today":
		start, end, err = aggregate.ResolveRange(aggregate.RangeToday, now, rangeRequest.TimeZone, 0)
		end = now
	case "yesterday":
		start, end, err = aggregate.ResolveRange(aggregate.RangeYesterday, now, rangeRequest.TimeZone, 0)
	case "this_week":
		start, end, err = aggregate.ResolveRange(aggregate.RangeCalendarWeek, now, rangeRequest.TimeZone, 0)
		end = now
	case "this_month":
		start = time.Date(localNow.Year(), localNow.Month(), 1, 0, 0, 0, 0, location)
		end = now
	case "last_n_hours":
		start, end, err = aggregate.ResolveRange(aggregate.RangeRolling, now, rangeRequest.TimeZone, time.Duration(rangeRequest.N)*time.Hour)
	case "last_n_days":
		start, end, err = aggregate.ResolveRange(aggregate.RangeRolling, now, rangeRequest.TimeZone, time.Duration(rangeRequest.N)*24*time.Hour)
	case "custom":
		start, end = rangeRequest.Start, rangeRequest.End
	default:
		return fmt.Errorf("unsupported range preset %q", rangeRequest.Preset)
	}
	if err != nil {
		return err
	}
	if !start.Before(end) {
		return fmt.Errorf("range start must be before end")
	}
	query.Start, query.End, query.TimeZone = start.UTC(), end.UTC(), rangeRequest.TimeZone
	// Storage readers consume the normalized v1-compatible bounds. Keeping the
	// named form alongside populated bounds would violate Query.Validate's
	// mutually-exclusive range representation.
	query.Range = nil
	return nil
}

func decodeAnalyticsJSON(c *gin.Context, target any, limit int64) error {
	data, err := readBoundedBody(c, limit)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(target); err != nil {
		return err
	}
	return requireAnalyticsJSONEOF(decoder)
}

func readBoundedBody(c *gin.Context, limit int64) ([]byte, error) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return nil, fmt.Errorf("request body is required")
	}
	data, err := io.ReadAll(io.LimitReader(c.Request.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read request body")
	}
	if len(data) == 0 || int64(len(data)) > limit {
		return nil, fmt.Errorf("request body exceeds its bounds")
	}
	return data, nil
}

func requireAnalyticsJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("request must contain one JSON value")
	}
	return nil
}

func setAnalyticsNoStore(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("Pragma", "no-cache")
	c.Header("Referrer-Policy", "no-referrer")
}

func writeAnalyticsInvalid(c *gin.Context, _ error) {
	writeAnalyticsEnvelope(c, http.StatusBadRequest, model.ErrorAnalyticsInvalidQuery, "The analytics query is invalid.")
}

func writeAnalyticsError(c *gin.Context, err error) {
	status, code, message := analyticsErrorStatus(err)
	writeAnalyticsEnvelope(c, status, code, message)
}

// WriteAnalyticsAPIError lets the viewer router reuse the frozen error
// envelope without exposing internal error text.
func WriteAnalyticsAPIError(c *gin.Context, err error) {
	writeAnalyticsError(c, err)
}

func WriteAnalyticsInvalidQuery(c *gin.Context) {
	writeAnalyticsInvalid(c, nil)
}

func WriteAnalyticsThrottled(c *gin.Context) {
	writeAnalyticsEnvelope(c, http.StatusTooManyRequests, model.ErrorAnalyticsThrottled, "The analytics request was throttled.")
}

func SetAnalyticsNoStore(c *gin.Context) {
	setAnalyticsNoStore(c)
}

func analyticsErrorStatus(err error) (int, model.ErrorCode, string) {
	switch {
	case errors.Is(err, cpauk.ErrDisabled):
		return http.StatusNotFound, model.ErrorAnalyticsDisabled, "Analytics is disabled."
	case errors.Is(err, cpauk.ErrUnavailable), errors.Is(err, cpauk.ErrClosed):
		return http.StatusServiceUnavailable, model.ErrorAnalyticsUnavailable, "Analytics is unavailable."
	case errors.Is(err, maintenance.ErrJobRunning):
		return http.StatusConflict, model.ErrorAnalyticsMaintenance, "Analytics maintenance is in progress."
	case errors.Is(err, cpauk.ErrMaintenance):
		return http.StatusServiceUnavailable, model.ErrorAnalyticsMaintenance, "Analytics maintenance is in progress."
	case errors.Is(err, maintenance.ErrJobNotFound), errors.Is(err, ErrViewerNotFound):
		return http.StatusNotFound, model.ErrorAnalyticsInvalidQuery, "The analytics resource was not found."
	case errors.Is(err, ErrViewerCredentialInvalid), errors.Is(err, ErrViewerSessionInvalid):
		return http.StatusUnauthorized, model.ErrorAnalyticsInvalidQuery, "The viewer credential is invalid."
	case errors.Is(err, ErrViewerViewForbidden):
		return http.StatusForbidden, model.ErrorAnalyticsInvalidQuery, "This viewer cannot access the requested view."
	case errors.Is(err, ErrViewerCapacity):
		return http.StatusTooManyRequests, model.ErrorAnalyticsThrottled, "The analytics request was throttled."
	case errors.Is(err, cpauk.ErrInternal):
		return http.StatusInternalServerError, model.ErrorAnalyticsInternal, "The analytics request failed."
	case errors.Is(err, errAnalyticsRetainedTimeZonePartial):
		message := "Retained analytics are stored in a different time zone and cannot be rebucketed exactly."
		var detailed retainedTimeZoneReadError
		if errors.As(err, &detailed) && detailed.detail != "" {
			message += " " + detailed.detail
		}
		return http.StatusBadRequest, model.ErrorAnalyticsInvalidQuery, message
	case errors.Is(err, errAnalyticsInvalidRead):
		return http.StatusBadRequest, model.ErrorAnalyticsInvalidQuery, "The analytics query is invalid."
	default:
		return http.StatusInternalServerError, model.ErrorAnalyticsInternal, "The analytics request failed."
	}
}

func writeAnalyticsEnvelope(c *gin.Context, status int, code model.ErrorCode, message string) {
	setAnalyticsNoStore(c)
	requestID := ""
	if c != nil && c.Request != nil {
		requestID = coreusage.ProxyRequestIDFromContext(c.Request.Context())
		if requestID == "" {
			requestID = logging.GetRequestID(c.Request.Context())
		}
	}
	c.AbortWithStatusJSON(status, model.ErrorEnvelope{Error: model.ErrorBody{
		Code: code, Message: message, RequestID: requestID,
	}})
}

func classifyAnalyticsReadError(err error) error {
	if errors.Is(err, cpauk.ErrDisabled) || errors.Is(err, cpauk.ErrUnavailable) || errors.Is(err, cpauk.ErrInternal) || errors.Is(err, cpauk.ErrClosed) || errors.Is(err, cpauk.ErrMaintenance) {
		return err
	}
	if errors.Is(err, store.ErrRetainedTimeZonePartial) {
		detail := ""
		var zoneErr store.RetainedTimeZoneError
		if errors.As(err, &zoneErr) {
			detail = zoneErr.Detail()
		}
		return retainedTimeZoneReadError{detail: detail}
	}
	if errors.Is(err, store.ErrRetainedRangePartial) {
		return fmt.Errorf("%w", errAnalyticsInvalidRead)
	}
	message := err.Error()
	for _, prefix := range []string{
		"invalid cursor", "cursor does not", "cursor query", "unsupported cursor", "invalid events cursor",
		"invalid dimensions cursor", "invalid leaderboard cursor", "leaderboard cursor is stale", "key catalog cursor is stale",
	} {
		if strings.HasPrefix(message, prefix) {
			return fmt.Errorf("%w", errAnalyticsInvalidRead)
		}
	}
	return cpauk.ErrInternal
}

var errAnalyticsInvalidRead = errors.New("invalid analytics read query")
var errAnalyticsRetainedTimeZonePartial = errors.New("retained analytics time zone is partial")

// retainedTimeZoneReadError carries the store's zone/grain detail into the
// error envelope while staying matchable as errAnalyticsRetainedTimeZonePartial.
type retainedTimeZoneReadError struct {
	detail string
}

func (e retainedTimeZoneReadError) Error() string {
	if e.detail == "" {
		return errAnalyticsRetainedTimeZonePartial.Error()
	}
	return errAnalyticsRetainedTimeZonePartial.Error() + ": " + e.detail
}

func (e retainedTimeZoneReadError) Unwrap() error { return errAnalyticsRetainedTimeZonePartial }

func analyticsCSVCell(value string) string {
	if value == "" {
		return value
	}
	switch value[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + value
	default:
		return value
	}
}

func writeAnalyticsEventsCSV(ctx context.Context, writer io.Writer, reader cpauk.Reader, query model.Query, maxRows int) (int, error) {
	if query.Operation != model.OperationEvents {
		return 0, fmt.Errorf("exports support events queries only")
	}
	if maxRows < 1 || maxRows > model.MaxExportRows {
		return 0, fmt.Errorf("invalid export row limit")
	}
	csvWriter := csv.NewWriter(writer)
	if err := csvWriter.Write(analyticsEventExportColumns); err != nil {
		return 0, err
	}
	written := 0
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
			values := analyticsEventExportValues(event)
			row := make([]string, 0, len(analyticsEventExportColumns))
			for _, column := range analyticsEventExportColumns {
				row = append(row, analyticsExportValueString(values[column]))
			}
			for index := range row {
				row[index] = analyticsCSVCell(row[index])
			}
			if err = csvWriter.Write(row); err != nil {
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
		query.Cursor = page.Meta.NextCursor
		previousCursor = query.Cursor
	}
	csvWriter.Flush()
	return written, csvWriter.Error()
}

func analyticsExportValueString(value any) string {
	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case *string:
		if typed == nil {
			return ""
		}
		return *typed
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	case *time.Time:
		if typed == nil {
			return ""
		}
		return typed.UTC().Format(time.RFC3339Nano)
	case bool:
		return strconv.FormatBool(typed)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case *int:
		if typed == nil {
			return ""
		}
		return strconv.Itoa(*typed)
	case *int64:
		if typed == nil {
			return ""
		}
		return strconv.FormatInt(*typed, 10)
	case model.NanoUSD:
		return typed.String()
	case *model.NanoUSD:
		if typed == nil {
			return ""
		}
		return typed.String()
	default:
		return fmt.Sprint(typed)
	}
}

type modelExportTooLargeError struct{}

func (modelExportTooLargeError) Error() string { return "analytics export exceeds row limit" }
