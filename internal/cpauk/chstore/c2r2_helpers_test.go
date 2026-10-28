package chstore

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func c2r2OpenV2FixtureStore(t *testing.T) (*Store, []model.Event) {
	t.Helper()
	ctx := context.Background()
	input, output := model.NanoUSD(1_000_000_000), model.NanoUSD(1_000_000_000)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{
		ID: "v2-price", Model: "model-v2", InputPerMillion: &input, OutputPerMillion: &output,
		CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "v2-fixture",
	}}}
	database := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: book})
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	errorClass := "rate_limit"
	ttft := int64(10)
	events := []model.Event{
		c2r2V2Event(strings.Repeat("1", 32), strings.Repeat("a", 32), strings.Repeat("d", 64), start.Add(time.Minute), true, nil, 99, 0, ttft, 100),
		c2r2V2Event(strings.Repeat("2", 32), strings.Repeat("a", 32), strings.Repeat("d", 64), start.Add(6*time.Minute), false, &errorClass, 0, 20, ttft*2, 200),
		c2r2V2Event(strings.Repeat("3", 32), strings.Repeat("b", 32), strings.Repeat("e", 64), start.Add(11*time.Minute), true, nil, 0, 40, ttft*3, 300),
	}
	if err := database.WriteBatch(ctx, []model.Event{events[0], events[2]}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.WriteImportBatch(ctx, []model.Event{events[1]}, "batch-v2"); err != nil {
		t.Fatal(err)
	}
	return database, events
}

func c2r2V2Event(attemptID, requestID, keyID string, requestedAt time.Time, succeeded bool, errorClass *string, cached, cacheRead, ttft, latency int64) model.Event {
	status := 200
	if !succeeded {
		status = 429
	}
	return model.Event{
		SchemaVersion: model.EventSchemaVersion, AttemptID: attemptID, ProxyRequestID: requestID,
		RequestIDQuality: model.RequestIDObserved, KeyID: keyID, RequestedAt: requestedAt,
		Provider: "provider-v2", ExecutorType: "executor-v2", Model: "model-v2", EndpointClass: "responses",
		Succeeded: succeeded, UpstreamStatusCode: &status, ErrorClass: errorClass, LatencyMS: latency,
		TimeToFirstTokenMS: &ttft, Tokens: model.TokenUsage{Input: 100, Output: 100, Cached: cached,
			CacheRead: cacheRead, Total: 200, Schema: "normalized-v1", Quality: model.TokenQualityExact},
	}
}

func c2r2V2Query(operation model.Operation, start, end time.Time) model.Query {
	return model.Query{SchemaVersion: 2, Operation: operation, Start: start, End: end, TimeZone: "UTC"}
}
