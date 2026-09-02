package maintenance

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
)

func TestStoreOperationsRepriceSupportsDryRunAndResume(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, store.Config{
		Path:            filepath.Join(t.TempDir(), "analytics.db"),
		MaxStorageBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })

	requestedAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	event := model.Event{
		SchemaVersion: model.EventSchemaVersion,
		AttemptID:     strings.Repeat("a", 32), ProxyRequestID: strings.Repeat("b", 32),
		RequestIDQuality: model.RequestIDObserved, KeyID: strings.Repeat("c", 64),
		RequestedAt: requestedAt, Provider: "provider", ExecutorType: "executor",
		Model: "model", EndpointClass: "responses", Succeeded: true, LatencyMS: 10,
		Tokens: model.TokenUsage{Input: 100, Output: 100, Total: 200, Schema: "normalized-v1", Quality: model.TokenQualityExact},
	}
	if err := database.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	event.AttemptID = strings.Repeat("d", 32)
	event.ProxyRequestID = strings.Repeat("e", 32)
	event.RequestedAt = requestedAt.Add(30 * time.Second)
	if err := database.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	input, output := model.NanoUSD(1_000_000_000), model.NanoUSD(1_000_000_000)
	if _, err := database.UpdatePriceBook(ctx, aggregate.PriceBook{Rules: []aggregate.PricingRule{{
		ID: "rule", Model: "model", InputPerMillion: &input, OutputPerMillion: &output,
		CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "test",
	}}}); err != nil {
		t.Fatal(err)
	}

	operation := StoreOperations(database)["reprice"]
	if operation == nil {
		t.Fatal("reprice operation is missing")
	}
	rangeValue := model.Range{Start: requestedAt.Add(-time.Minute), End: requestedAt.Add(time.Minute), TimeZone: "UTC"}
	dryRun, err := operation(ctx, map[string]any{"range": rangeValue, "dry_run": true, "chunk_size": 1}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ := dryRun["checkpoint"].(string)
	if dryRun["matched"] != int64(2) || dryRun["updated"] != int64(0) || dryRun["completed"] != true || checkpoint == "" {
		t.Fatalf("dry-run result = %+v", dryRun)
	}

	committed, err := operation(ctx, map[string]any{
		"range": rangeValue, "chunk_size": 1,
	}, func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if committed["updated"] != int64(2) || committed["completed"] != true {
		t.Fatalf("resumed result = %+v", committed)
	}
	if committed["history_complete"] != true || committed["retained_cutoff"] != (*time.Time)(nil) {
		t.Fatalf("raw-only reprice coverage = %+v", committed)
	}
	effectiveStart, ok := committed["effective_start"].(time.Time)
	if !ok || !effectiveStart.Equal(rangeValue.Start) {
		t.Fatalf("raw-only effective start = %#v", committed["effective_start"])
	}
}
