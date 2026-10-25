package management

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestAnalyticsExportsRetainRecordedEventFields(t *testing.T) {
	stamp := time.Date(2026, 8, 1, 0, 0, 0, 123456789, time.UTC)
	zero := int64(0)
	const rawPrefix = `{"usage":{"total_tokens":42},"note":"界,\"`
	const rawSuffix = `"}`
	raw := model.RawJSON(rawPrefix + strings.Repeat("x", 40960-len(rawPrefix)-len(rawSuffix)) + rawSuffix)
	event := model.Event{RequestedAt: stamp, FirstTokenLatencyMS: &zero, ProviderLatencyMS: &zero, RoutingTimeMS: &zero, GenerationTimeMS: &zero, UpstreamUsageRaw: &raw, ReceivedAt: &stamp, Tokens: model.TokenUsage{Total: 42}}
	event.LatencyMS = 1000
	event.Stream = new(false)
	effort := "high"
	event.ReasoningEffort = &effort
	event.Tokens = model.TokenUsage{Output: 42, Total: 42, Schema: model.AccountingSchemaNormalizedV1, Quality: model.TokenQualityExact}
	event.OutputTiming = &model.OutputTiming{FirstBatchBytes: 10, TotalBytes: 100, LargestBatchBytes: 50, MaxInterarrivalNS: 700000000, Version: 1, Protocol: "kiro", Source: "source_reader", Status: "observed", FirstArrivalNS: 20000000001, LastArrivalNS: 21000000001, ArrivalCount: 3, WindowTokens: 32, TokenQuality: model.TokenQualityEstimated, Coverage: "wire_deltas"}
	event.EnrichThroughput()
	// Populate every optional recorded field so omitempty cannot conceal omissions.
	value := reflect.ValueOf(&event).Elem()
	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)
		if field.Kind() == reflect.Pointer && field.IsNil() {
			field.Set(reflect.New(field.Type().Elem()))
		}
	}
	canonicalBytes, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var canonical map[string]json.RawMessage
	if err = json.Unmarshal(canonicalBytes, &canonical); err != nil {
		t.Fatal(err)
	}
	reader := &analyticsHandlerReader{events: model.EventPage{Events: []model.Event{event}}}
	handler := &Handler{analytics: &analyticsHandlerService{reader: reader, state: model.StateReady}}
	export := func(format string) []byte {
		ctx, recorder := v2Request(http.MethodPost, "/v0/management/analytics/exports", `{"query":{"schema_version":2,"operation":"events","start":"2026-08-01T00:00:00Z","end":"2026-08-02T00:00:00Z","time_zone":"UTC"},"format":"`+format+`"}`)
		handler.CreateAnalyticsExport(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("export %s status=%d: %s", format, recorder.Code, recorder.Body.String())
		}
		return recorder.Body.Bytes()
	}
	output := bytes.NewBuffer(export("json"))
	var exported []map[string]json.RawMessage
	if err = json.Unmarshal(output.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	for key, want := range canonical {
		got, ok := exported[0][key]
		if !ok || !bytes.Equal(got, want) {
			t.Errorf("JSON field %s missing or changed", key)
		}
	}
	output = bytes.NewBuffer(export("csv"))
	rows, err := csv.NewReader(output).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	cells := map[string]string{}
	for i, key := range rows[0] {
		cells[key] = rows[1][i]
	}
	for key, want := range canonical {
		got, ok := cells[key]
		if !ok {
			t.Errorf("CSV missing %s", key)
			continue
		}
		var text string
		if json.Unmarshal(want, &text) == nil {
			if got != text {
				t.Errorf("CSV string %s changed", key)
			}
		} else if got != string(want) {
			t.Errorf("CSV JSON/scalar %s changed: %.80s", key, got)
		}
	}
	if cells["total_tokens"] != "42" || cells["routing_time_ms"] != "0" || cells["tokens_per_second"] != "32" || cells["effective_output_tokens_per_second"] != "42" || cells["speed_estimated"] != "true" || cells["stream"] != "false" || cells["reasoning_effort"] != "high" {
		t.Fatal("legacy totals or zero timing lost")
	}
}

func TestAnalyticsExportNullDiagnosticsStayNull(t *testing.T) {
	reader := &analyticsHandlerReader{events: model.EventPage{Events: []model.Event{{}}}}
	query := model.Query{Operation: model.OperationEvents}
	var output bytes.Buffer
	if _, err := writeAnalyticsEventsJSON(context.Background(), &output, reader, query, 1); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"upstream_usage_raw", "upstream_error_body", "routing_time_ms", "first_token_latency_ms", "provider_latency_ms", "received_at", "proxy_status_code"} {
		if string(rows[0][key]) != "null" {
			t.Errorf("%s should remain null", key)
		}
	}
	output.Reset()
	if _, err := writeAnalyticsEventsCSV(context.Background(), &output, reader, query, 1); err != nil {
		t.Fatal(err)
	}
	cells, err := csv.NewReader(&output).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for i, key := range cells[0] {
		if strings.HasSuffix(key, "_raw") || key == "routing_time_ms" || key == "received_at" {
			if cells[1][i] != "" {
				t.Errorf("CSV %s should remain blank", key)
			}
		}
	}
}

func TestAnalyticsEstimateExportsKeepPredictionSeparate(t *testing.T) {
	rate, low, high := 1500.0, 1200.0, 1700.0
	event := model.Event{ThroughputInfo: &model.ThroughputInfo{Status: "unavailable", Method: "arrival_window_v1", Reason: "insufficient_tokens"}, ThroughputEstimate: &model.ThroughputEstimate{Status: "estimated", Method: "cohort_median_v1", TokensPerSecond: &rate, HistoricalP10: &low, HistoricalP90: &high, TrainingSamples: 50, ValidationSamples: 20}}
	reader := &analyticsHandlerReader{events: model.EventPage{Events: []model.Event{event}}}
	var output bytes.Buffer
	if _, err := writeAnalyticsEventsJSON(context.Background(), &output, reader, model.Query{Operation: model.OperationEvents}, 1); err != nil {
		t.Fatal(err)
	}
	var exported []map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	var estimate model.ThroughputEstimate
	if err := json.Unmarshal(exported[0]["throughput_estimate"], &estimate); err != nil || !reflect.DeepEqual(estimate, *event.ThroughputEstimate) {
		t.Fatalf("estimate=%+v err=%v", estimate, err)
	}
	if string(exported[0]["tokens_per_second"]) != "null" || string(exported[0]["speed_estimated"]) != "false" {
		t.Fatalf("prediction leaked into aliases: %s", output.String())
	}
	output.Reset()
	if _, err := writeAnalyticsEventsCSV(context.Background(), &output, reader, model.Query{Operation: model.OperationEvents}, 1); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&output).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range rows[0] {
		if name == "throughput_estimate" {
			if err := json.Unmarshal([]byte(rows[1][i]), &estimate); err != nil || !reflect.DeepEqual(estimate, *event.ThroughputEstimate) {
				t.Fatalf("CSV estimate=%+v err=%v", estimate, err)
			}
		}
		if name == "tokens_per_second" && rows[1][i] != "" {
			t.Fatal("CSV prediction leaked into observed scalar")
		}
	}
}
