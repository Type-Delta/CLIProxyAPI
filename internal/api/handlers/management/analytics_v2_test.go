package management

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
)

type analyticsV2ReaderFake struct {
	*analyticsHandlerReader
	activity model.Activity
	analysis model.Analysis
}

func (r *analyticsV2ReaderFake) Activity(_ context.Context, query model.Query) (model.Activity, error) {
	r.query = query
	return r.activity, r.err
}

func (r *analyticsV2ReaderFake) Analysis(_ context.Context, query model.Query) (model.Analysis, error) {
	r.query = query
	return r.analysis, r.err
}

type analyticsV2ServiceFake struct {
	*analyticsHandlerService
	v2          *analyticsV2ReaderFake
	maintenance cpauk.Maintenance
}

func (s *analyticsV2ServiceFake) Reader() cpauk.Reader { return s.v2 }

func (s *analyticsV2ServiceFake) Maintenance() cpauk.Maintenance {
	if s.maintenance != nil {
		return s.maintenance
	}
	return s.analyticsHandlerService.Maintenance()
}

type analyticsV2MaintenanceFake struct {
	request cpauk.MaintenanceRequest
	status  model.JobStatus
}

func (m *analyticsV2MaintenanceFake) Start(_ context.Context, request cpauk.MaintenanceRequest) (model.JobStatus, error) {
	m.request = request
	if m.status.JobID == "" {
		m.status = model.JobStatus{JobID: "job-v2", Kind: request.Kind, State: model.JobQueued}
	}
	return m.status, nil
}

func (m *analyticsV2MaintenanceFake) Status(context.Context, string) (model.JobStatus, error) {
	return m.status, nil
}

func (*analyticsV2MaintenanceFake) Cancel(context.Context, string) error { return nil }

func v2HandlerWithReader(reader *analyticsV2ReaderFake, maintenance cpauk.Maintenance) *Handler {
	return &Handler{analytics: &analyticsV2ServiceFake{
		analyticsHandlerService: &analyticsHandlerService{reader: reader.analyticsHandlerReader, state: model.StateReady},
		v2:                      reader, maintenance: maintenance,
	}}
}

func v2Request(method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	return ctx, recorder
}

func TestPostAnalyticsQueryV2NamedRangesResolveAndEchoBounds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reader := &analyticsV2ReaderFake{analyticsHandlerReader: &analyticsHandlerReader{}}
	handler := v2HandlerWithReader(reader, nil)

	ctx, recorder := v2Request(http.MethodPost, "/v0/management/analytics/query", `{"schema_version":2,"operation":"activity","range":{"preset":"last_n_days","n":7,"time_zone":"UTC"},"window":"day"}`)
	handler.PostAnalyticsQuery(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("last_n_days status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := reader.query.End.Sub(reader.query.Start); got != 7*24*time.Hour || reader.query.TimeZone != "UTC" {
		t.Fatalf("resolved last_n_days query=%+v duration=%s", reader.query, got)
	}
	if !strings.Contains(recorder.Body.String(), `"range"`) || !strings.Contains(recorder.Body.String(), `"UTC"`) {
		t.Fatalf("resolved range was not echoed: %s", recorder.Body.String())
	}

	custom := `{"schema_version":2,"operation":"analysis","range":{"preset":"custom","start":"2026-08-01T00:00:00Z","end":"2026-08-08T00:00:00Z","time_zone":"Asia/Kolkata"}}`
	ctx, recorder = v2Request(http.MethodPost, "/v0/management/analytics/query", custom)
	handler.PostAnalyticsQuery(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("custom status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	wantStart := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	if !reader.query.Start.Equal(wantStart) || !reader.query.End.Equal(wantEnd) || reader.query.TimeZone != "Asia/Kolkata" {
		t.Fatalf("custom query=%+v", reader.query)
	}
	if !strings.Contains(recorder.Body.String(), "2026-08-01T00:00:00Z") || !strings.Contains(recorder.Body.String(), "Asia/Kolkata") {
		t.Fatalf("custom range was not echoed: %s", recorder.Body.String())
	}
}

func TestResolveAnalyticsRangeCurrentPeriodsEndAtNow(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 34, 56, 0, time.UTC)
	for _, preset := range []string{"today", "this_week", "this_month"} {
		t.Run(preset, func(t *testing.T) {
			query := model.Query{SchemaVersion: 2, Operation: model.OperationSummary, Range: &model.RangeRequest{Preset: preset, TimeZone: "UTC"}}
			if err := resolveAnalyticsRange(&query, now); err != nil {
				t.Fatal(err)
			}
			if !query.End.Equal(now) {
				t.Fatalf("%s end = %s, want %s", preset, query.End, now)
			}
		})
	}

	yesterday := model.Query{SchemaVersion: 2, Operation: model.OperationSummary, Range: &model.RangeRequest{Preset: "yesterday", TimeZone: "UTC"}}
	if err := resolveAnalyticsRange(&yesterday, now); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC); !yesterday.End.Equal(want) {
		t.Fatalf("yesterday end = %s, want %s", yesterday.End, want)
	}
}

func TestPostAnalyticsQueryV2DispatchesActivityAndAnalysis(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reader := &analyticsV2ReaderFake{analyticsHandlerReader: &analyticsHandlerReader{}}
	handler := v2HandlerWithReader(reader, nil)
	for _, operation := range []model.Operation{model.OperationActivity, model.OperationAnalysis} {
		window := ""
		if operation == model.OperationActivity {
			window = `,"window":"day"`
		}
		body := `{"schema_version":2,"operation":"` + string(operation) + `","start":"2026-08-01T00:00:00Z","end":"2026-08-01T01:00:00Z","time_zone":"UTC"` + window + `}`
		ctx, recorder := v2Request(http.MethodPost, "/v0/management/analytics/query", body)
		handler.PostAnalyticsQuery(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", operation, recorder.Code, recorder.Body.String())
		}
		if reader.query.Operation != operation {
			t.Fatalf("dispatched operation=%q, want %q", reader.query.Operation, operation)
		}
	}
}

func TestGetAnalyticsKeysPreservesExplicitRangeAndConfiguredIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rawKey := "configured-key-v2"
	keyID := config.APIKeyID(rawKey)
	reader := &analyticsV2ReaderFake{analyticsHandlerReader: &analyticsHandlerReader{
		dimensions: model.DimensionPage{Rows: []model.DimensionRow{{Value: keyID}}},
	}}
	handler := v2HandlerWithReader(reader, nil)
	handler.cfg = &config.Config{SDKConfig: config.SDKConfig{APIKeys: []config.APIKeyEntry{{Key: rawKey}}}}
	ctx, recorder := v2Request(http.MethodGet, "/v0/management/analytics/keys?start=2026-08-01T00:00:00Z&end=2026-08-08T00:00:00Z&time_zone=UTC", "")
	handler.GetAnalyticsKeys(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !reader.query.Start.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) || !reader.query.End.Equal(time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("key range was not preserved: %+v", reader.query)
	}
	if !strings.Contains(recorder.Body.String(), `"status":"configured"`) || strings.Contains(recorder.Body.String(), rawKey) {
		t.Fatalf("configured identity enrichment leaked or was omitted: %s", recorder.Body.String())
	}
}

func TestPostAnalyticsEventsAcceptsResultErrorAndSourceFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reader := &analyticsV2ReaderFake{analyticsHandlerReader: &analyticsHandlerReader{}}
	handler := v2HandlerWithReader(reader, nil)
	body := `{"schema_version":2,"operation":"events","start":"2026-08-01T00:00:00Z","end":"2026-08-02T00:00:00Z","time_zone":"UTC","filters":{"result":"failure","error_class":["rate_limit"],"source":"import"}}`
	ctx, recorder := v2Request(http.MethodPost, "/v0/management/analytics/query", body)
	handler.PostAnalyticsQuery(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if string(reader.query.Filters["result"]) != `"failure"` || string(reader.query.Filters["source"]) != `["import"]` || string(reader.query.Filters["error_class"]) != `["rate_limit"]` {
		t.Fatalf("event filters=%s", mustJSON(reader.query.Filters))
	}
}

func TestAnalyticsRetainedCrossZoneErrorIsTruthful(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reader := &analyticsV2ReaderFake{analyticsHandlerReader: &analyticsHandlerReader{
		err: fmt.Errorf("%w: %w", store.ErrRetainedRangePartial, store.ErrRetainedTimeZonePartial),
	}}
	handler := v2HandlerWithReader(reader, nil)
	body := `{"schema_version":2,"operation":"summary","start":"2026-08-01T00:00:00Z","end":"2026-08-02T00:00:00Z","time_zone":"Asia/Kolkata"}`
	ctx, recorder := v2Request(http.MethodPost, "/v0/management/analytics/query", body)
	handler.PostAnalyticsQuery(ctx)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "cannot be rebucketed exactly") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAnalyticsRetainedCrossZoneErrorKeepsStoreDetail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reader := &analyticsV2ReaderFake{analyticsHandlerReader: &analyticsHandlerReader{
		err: fmt.Errorf("query analytics timeseries: %w", store.RetainedTimeZoneError{
			StorageTimeZone: "Asia/Bangkok", QueryTimeZone: "Asia/Kolkata", BucketWidth: "1h",
		}),
	}}
	handler := v2HandlerWithReader(reader, nil)
	body := `{"schema_version":2,"operation":"timeseries","start":"2026-08-01T00:00:00Z","end":"2026-08-02T00:00:00Z","time_zone":"Asia/Kolkata","bucket_width":"1h"}`
	ctx, recorder := v2Request(http.MethodPost, "/v0/management/analytics/query", body)
	handler.PostAnalyticsQuery(ctx)
	responseBody := recorder.Body.String()
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, responseBody)
	}
	for _, want := range []string{"cannot be rebucketed exactly", "Asia/Bangkok", "Asia/Kolkata", "1h"} {
		if !strings.Contains(responseBody, want) {
			t.Fatalf("cross-zone reason is missing %q: %s", want, responseBody)
		}
	}
	if strings.Contains(responseBody, "query analytics timeseries") {
		t.Fatalf("internal error text leaked: %s", responseBody)
	}
}

func TestAnalyticsExportRejectsCursorAndSupportsSanitizedCSVAndJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	keyID := strings.Repeat("a", 64)
	errorClass := "rate_limit"
	alias := "requested"
	authType := "oauth"
	credentialID := strings.Repeat("b", 64)
	ruleID := "rule-v2"
	priceSource := "catalog-v2"
	importBatch := "batch-v2"
	source := "import"
	ttft := int64(25)
	knownCost := model.NanoUSD(500)
	event := model.Event{SchemaVersion: model.EventSchemaVersion, AttemptID: strings.Repeat("1", 32), ProxyRequestID: strings.Repeat("2", 32), RequestIDQuality: model.RequestIDObserved, KeyID: keyID, RequestedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Provider: "provider", ExecutorType: "executor", Model: "model", RequestedAlias: &alias, EndpointClass: "responses", AuthType: &authType, CredentialID: &credentialID, CredentialIDAlgorithm: func() *string { value := model.CredentialIDAlgorithm; return &value }(), Succeeded: false, ErrorClass: &errorClass, TimeToFirstTokenMS: &ttft, Generated: true, Tokens: model.TokenUsage{Input: 10, Output: 20, CacheRead: 4, CacheCreation: 2, Total: 30, Schema: "normalized-v1", Quality: model.TokenQualityExact}, KnownCost: &knownCost, PriceRuleID: ruleID, PriceSource: priceSource, ImportBatchID: importBatch, Source: source}
	reader := &analyticsV2ReaderFake{analyticsHandlerReader: &analyticsHandlerReader{events: model.EventPage{Events: []model.Event{event}}}}
	handler := v2HandlerWithReader(reader, nil)
	baseQuery := `{"schema_version":2,"operation":"events","start":"2026-08-01T00:00:00Z","end":"2026-08-02T00:00:00Z","time_zone":"UTC"}`

	queryWithCursor := strings.TrimSuffix(baseQuery, "}") + `,"cursor":"not-allowed"}`
	ctx, recorder := v2Request(http.MethodPost, "/v0/management/analytics/exports", `{"query":`+queryWithCursor+`}`)
	handler.CreateAnalyticsExport(ctx)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("cursor export status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	v1NamedRange := `{"schema_version":1,"operation":"events","range":{"preset":"today","time_zone":"UTC"}}`
	ctx, recorder = v2Request(http.MethodPost, "/v0/management/analytics/exports", `{"query":`+v1NamedRange+`}`)
	handler.CreateAnalyticsExport(ctx)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("v1 named-range export status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var csvFields, jsonFields []string
	for _, format := range []string{"csv", "json"} {
		ctx, recorder = v2Request(http.MethodPost, "/v0/management/analytics/exports", `{"query":`+baseQuery+`,"format":"`+format+`"}`)
		handler.CreateAnalyticsExport(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s export status=%d body=%s", format, recorder.Code, recorder.Body.String())
		}
		for _, forbidden := range []string{"api_key", "request_body", "request_headers", "ip_address", "forwarded_for", "user_agent", "auth_id", "auth_index"} {
			if strings.Contains(recorder.Body.String(), forbidden) {
				t.Fatalf("%s export contains forbidden field %q: %s", format, forbidden, recorder.Body.String())
			}
		}
		if format == "csv" {
			rows, err := csv.NewReader(strings.NewReader(recorder.Body.String())).ReadAll()
			if err != nil || len(rows) < 2 {
				t.Fatalf("csv rows=%v err=%v", rows, err)
			}
			for _, required := range []string{"attempt_id", "requested_at", "provider", "model", "latency_ms", "time_to_first_token_ms", "cache_read_tokens", "known_cost_usd", "unpriced_tokens", "price_rule_id", "price_source", "import_batch_id", "source"} {
				if !strings.Contains(strings.Join(rows[0], ","), required) {
					t.Fatalf("csv header omitted %q: %v", required, rows[0])
				}
			}
			csvFields = append(csvFields, rows[0]...)
		} else {
			var payload []map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil || len(payload) != 1 {
				t.Fatalf("json export payload=%s err=%v", recorder.Body.String(), err)
			}
			for _, required := range []string{"attempt_id", "requested_at", "provider", "model", "latency_ms", "time_to_first_token_ms", "cache_read_tokens", "known_cost_usd", "unpriced_tokens", "price_rule_id", "price_source", "import_batch_id", "source"} {
				if _, ok := payload[0][required]; !ok {
					t.Fatalf("json export omitted %q: %v", required, payload[0])
				}
			}
			for field := range payload[0] {
				jsonFields = append(jsonFields, field)
			}
			slices.Sort(jsonFields)
			if payload[0]["price_rule_id"] != ruleID || payload[0]["price_source"] != priceSource || payload[0]["import_batch_id"] != importBatch || payload[0]["source"] != source {
				t.Fatalf("json export lost provenance: %v", payload[0])
			}
		}
	}
	slices.Sort(csvFields)
	if !slices.Equal(csvFields, jsonFields) {
		t.Fatalf("export field mismatch\n csv: %v\njson: %v", csvFields, jsonFields)
	}
}

func TestAnalyticsPricingValidatesMetadataAndReportsMissingProvenance(t *testing.T) {
	gin.SetMode(gin.TestMode)
	input, output := model.NanoUSD(1), model.NanoUSD(2)
	service := &analyticsPricingTestService{analyticsHandlerService: &analyticsHandlerService{reader: &analyticsHandlerReader{}, state: model.StateReady}, book: aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "rule-v2", Model: "model-v2", InputPerMillion: &input, OutputPerMillion: &output, Source: "catalog-v2"}}}}
	handler := &Handler{analytics: service}
	for _, field := range []string{"currency_unit", "rounding"} {
		body := `{"` + field + `":"wrong","rules":[]}`
		ctx, recorder := v2Request(http.MethodPut, "/v0/management/analytics/pricing", body)
		handler.PutAnalyticsPricing(ctx)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("invalid %s status=%d body=%s", field, recorder.Code, recorder.Body.String())
		}
	}
	ctx, recorder := v2Request(http.MethodGet, "/v0/management/analytics/pricing", "")
	handler.GetAnalyticsPricing(ctx)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"source":"catalog-v2"`) || !strings.Contains(recorder.Body.String(), `"missing"`) {
		t.Fatalf("pricing response status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPostAnalyticsRepriceStartsResumableJobWithResolvedOptions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reader := &analyticsV2ReaderFake{analyticsHandlerReader: &analyticsHandlerReader{}}
	maintenance := &analyticsV2MaintenanceFake{}
	handler := v2HandlerWithReader(reader, maintenance)
	body := `{"range":{"preset":"custom","start":"2026-08-01T00:00:00Z","end":"2026-08-02T00:00:00Z","time_zone":"UTC"},"dry_run":true,"resume":true}`
	ctx, recorder := v2Request(http.MethodPost, "/v0/management/analytics/pricing/reprice", body)
	handler.PostAnalyticsReprice(ctx)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if maintenance.request.Kind != "reprice" || maintenance.request.Options["dry_run"] != true || maintenance.request.Options["resume"] != true {
		t.Fatalf("reprice request=%+v", maintenance.request)
	}
	if got, ok := maintenance.request.Options["range"].(model.Range); !ok || !got.Start.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) || !got.End.Equal(time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("reprice range=%#v", maintenance.request.Options["range"])
	}
}

type analyticsV2ProviderQuotaService struct {
	*analyticsV2ServiceFake
	snapshots []store.ProviderQuotaSnapshot
}

func (s *analyticsV2ProviderQuotaService) CredentialID(_, _, _ string) (*string, error) {
	id := strings.Repeat("c", 64)
	return &id, nil
}

func (s *analyticsV2ProviderQuotaService) ReplaceProviderQuotaSnapshots(_ context.Context, snapshots []store.ProviderQuotaSnapshot) error {
	s.snapshots = snapshots
	return nil
}

func (s *analyticsV2ProviderQuotaService) ProviderQuotaSnapshots(context.Context) ([]store.ProviderQuotaSnapshot, error) {
	return s.snapshots, nil
}

func TestAnalyticsProvidersAndQuotasExposeCredentialRowsWithoutRawAuthIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rawAuthID := "raw-oauth-secret"
	credentialID := strings.Repeat("c", 64)
	reader := &analyticsV2ReaderFake{analyticsHandlerReader: &analyticsHandlerReader{}}
	service := &analyticsV2ProviderQuotaService{
		analyticsV2ServiceFake: v2HandlerWithReader(reader, nil).analytics.(*analyticsV2ServiceFake),
		snapshots:              []store.ProviderQuotaSnapshot{{Provider: "provider-v2", CredentialID: credentialID, Model: "model-v2", Available: true, ObservedAt: time.Now().UTC()}},
	}
	handler := &Handler{analytics: service}
	for _, endpoint := range []func(*gin.Context){handler.GetAnalyticsProviders, handler.GetAnalyticsQuotas} {
		ctx, recorder := v2Request(http.MethodGet, "/", "")
		endpoint(ctx)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), credentialID) || strings.Contains(recorder.Body.String(), rawAuthID) {
			t.Fatalf("credential response status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

func mustJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

var _ = io.EOF
