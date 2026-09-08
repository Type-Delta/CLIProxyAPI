package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func TestTimingCoverageAggregatesAndImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	database, events := openV2FixtureStore(t)
	start := events[0].RequestedAt
	query := v2Query(model.OperationAnalysis, start, start.Add(time.Hour))
	historical, err := database.Analysis(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	metric := historical.Latency.Metrics["generation"]
	if metric.SampleCount != 0 || metric.TotalMS != nil || metric.MedianMS != nil {
		t.Fatalf("historical generation fabricated: %+v", metric)
	}
	for index, value := range []int64{0, 40, 80, 100} {
		event := events[0]
		event.AttemptID = strings.Repeat(string(rune('4'+index)), 32)
		event.RequestedAt = start.Add(time.Duration(index+1) * time.Second)
		event.GenerationTimeMS = &value
		// Exercise sanitized JSON replay and import persistence, including measured zero.
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		var replay model.Event
		if err := json.Unmarshal(encoded, &replay); err != nil {
			t.Fatal(err)
		}
		if _, err := database.WriteImportBatch(ctx, []model.Event{replay}, "generation-replay"); err != nil {
			t.Fatal(err)
		}
	}
	analysis, err := database.Analysis(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	metric = analysis.Latency.Metrics["generation"]
	if metric.SampleCount != 4 || metric.TotalMS == nil || *metric.TotalMS != 220 || metric.MedianMS == nil || *metric.MedianMS != 60 || metric.P95MS == nil || *metric.P95MS != 100 || metric.MaxMS == nil || *metric.MaxMS != 100 {
		t.Fatalf("generation metrics=%+v", metric)
	}
	for _, key := range []string{"latency", "provider_latency"} {
		value := analysis.Latency.Metrics[key]
		if value.Source != "unavailable" || value.TotalMS != nil || value.SampleCount != 0 {
			t.Fatalf("provider timing fabricated: %+v", value)
		}
	}
	summaryQuery := query
	summaryQuery.Operation = model.OperationSummary
	summary, err := database.Summary(ctx, summaryQuery)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProcessingTime.GenerationMS == nil || *summary.ProcessingTime.GenerationMS != 220 || summary.ProcessingTime.GenerationSampleCount != 4 || summary.ProcessingTime.SampleCount != 7 || !summary.ProcessingTime.Partial {
		t.Fatalf("processing time=%+v", summary.ProcessingTime)
	}
	keyQuery := query
	keyQuery.Operation = model.OperationDimensions
	keyQuery.Dimension = "key"
	keys, err := database.KeyCatalog(ctx, keyQuery)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, key := range keys.Keys {
		if key.KeyID == events[0].KeyID {
			found = true
			if key.Requests != 1 || key.TopModel == nil || *key.TopModel != "model-v2" || key.TopModelTokens != 1200 || key.GenerationTimeMS == nil || *key.GenerationTimeMS != 220 || key.GenerationSampleCount != 4 {
				t.Fatalf("key details=%+v", key)
			}
		}
	}
	if !found {
		t.Fatal("key missing")
	}
	for _, cell := range analysis.KeyModelMatrix.Cells {
		if cell.KeyID == events[0].KeyID {
			if cell.GenerationTimeMS == nil || *cell.GenerationTimeMS != 220 || cell.GenerationSampleCount != 4 {
				t.Fatalf("matrix=%+v", cell)
			}
		}
	}
	var cost model.NanoUSD
	for _, components := range analysis.CostComponents.Models {
		encoded, _ := json.Marshal(components.TotalUSD)
		var total model.NanoUSD
		if err := json.Unmarshal(encoded, &total); err != nil {
			t.Fatal(err)
		}
		cost += total
	}
	if cost != summary.KnownCost {
		t.Fatalf("model components cost=%s summary=%s", cost, summary.KnownCost)
	}
}

func TestTimingSupportsLongRangeAndOddMedian(t *testing.T) {
	database, events := openV2FixtureStore(t)
	ctx := context.Background()
	if _, err := database.db.ExecContext(ctx, `UPDATE events SET generation_time_ms=CASE latency_ms WHEN 100 THEN 0 WHEN 200 THEN 40 ELSE 80 END`); err != nil {
		t.Fatal(err)
	}
	query := v2Query(model.OperationAnalysis, events[0].RequestedAt, events[0].RequestedAt.Add(31*24*time.Hour))
	analysis, err := database.Analysis(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	metric := analysis.Latency.Metrics["generation"]
	if metric.MedianMS == nil || *metric.MedianMS != 40 || metric.TotalMS == nil || *metric.TotalMS != 120 {
		t.Fatalf("long-range timing=%+v", metric)
	}
}
