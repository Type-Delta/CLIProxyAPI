package store

// Opt-in benchmark for the historical result cache and sealed closed buckets.
// It deliberately uses the deterministic compact benchmark fixture so cache
// and storage changes can be compared on the same 100k-request history.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

type segmentBenchmarkResult struct {
	Label         string            `json:"label"`
	GeneratedAt   string            `json:"generated_at"`
	Requests      int               `json:"requests"`
	DatabaseBytes int64             `json:"database_bytes"`
	Rows          map[string]int64  `json:"rows"`
	Scenarios     []segmentScenario `json:"scenarios"`
}

type segmentScenario struct {
	Name       string `json:"name"`
	DurationNS int64  `json:"duration_ns"`
	Output     int64  `json:"output,omitempty"`
	AllocBytes int64  `json:"alloc_bytes"`
	HeapBytes  uint64 `json:"heap_inuse_bytes"`
	Error      string `json:"error,omitempty"`
}

// TestCPAUKSegmentBenchmark runs deterministic cache/seal scenarios. Set
// CPAUK_SEGMENT_BENCHMARK=1 and CPAUK_BENCH_DIR to a compact fixture root.
func TestCPAUKSegmentBenchmark(t *testing.T) {
	if os.Getenv("CPAUK_SEGMENT_BENCHMARK") != "1" {
		t.Skip("set CPAUK_SEGMENT_BENCHMARK=1 to run segment benchmark")
	}
	root := os.Getenv("CPAUK_BENCH_DIR")
	if root == "" {
		t.Fatal("CPAUK_BENCH_DIR is required")
	}
	if _, err := os.Stat(filepath.Join(root, "analytics.db")); err != nil {
		t.Fatalf("fixture database: %v", err)
	}
	ctx := context.Background()
	end := compactBenchmarkEnd
	query := model.Query{SchemaVersion: model.QuerySchemaVersionV2, Operation: model.OperationSummary,
		Start: end.Add(-90 * 24 * time.Hour), End: end, TimeZone: "UTC"}
	result := segmentBenchmarkResult{Label: envOrDefault("CPAUK_BENCH_LABEL", "segment"), GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), Requests: 100000}
	store, err := compactBenchmarkOpen(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close(ctx) }()
	result.Rows, err = compactBenchmarkRowCounts(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	result.DatabaseBytes = compactBenchmarkDatabaseBytes(compactBenchmarkDatabaseFiles(root))
	measure := func(name string, fn func() (int64, error)) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		started := time.Now()
		output, runErr := fn()
		runtime.ReadMemStats(&after)
		scenario := segmentScenario{Name: name, DurationNS: time.Since(started).Nanoseconds(), Output: output, AllocBytes: int64(after.TotalAlloc - before.TotalAlloc), HeapBytes: after.HeapInuse}
		if runErr != nil {
			scenario.Error = runErr.Error()
		}
		result.Scenarios = append(result.Scenarios, scenario)
	}
	// First call populates the history cache; the immediate repeat is the hit.
	measure("cache_cold", func() (int64, error) { value, err := store.Summary(ctx, query); return value.UpstreamAttempts, err })
	measure("cache_hit", func() (int64, error) { value, err := store.Summary(ctx, query); return value.UpstreamAttempts, err })
	// A current tail write must leave the historical result reusable.
	if err := store.WriteBatch(ctx, []model.Event{segmentBenchmarkEvent(end.Add(time.Hour), "fffffffffffffffffffffffffffffff1")}); err != nil {
		t.Fatal(err)
	}
	measure("tail_write_history", func() (int64, error) { value, err := store.Summary(ctx, query); return value.UpstreamAttempts, err })
	// A late write inside the queried range invalidates the historical result.
	if err := store.WriteBatch(ctx, []model.Event{segmentBenchmarkEvent(end.Add(-24*time.Hour), "fffffffffffffffffffffffffffffff2")}); err != nil {
		t.Fatal(err)
	}
	measure("late_write_history", func() (int64, error) { value, err := store.Summary(ctx, query); return value.UpstreamAttempts, err })
	// Sealed and unsealed reads use separate stores and the same immutable fixture.
	sealed, err := compactBenchmarkOpen(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sealed.Close(ctx) }()
	measure("unsealed_summary", func() (int64, error) { value, err := sealed.Summary(ctx, query); return value.UpstreamAttempts, err })
	if sealer, ok := any(sealed).(interface {
		SealClosedSegments(context.Context, time.Time) (int64, error)
	}); ok {
		if _, err := sealer.SealClosedSegments(ctx, end); err != nil {
			t.Fatal(err)
		}
		measure("sealed_summary", func() (int64, error) { value, err := sealed.Summary(ctx, query); return value.UpstreamAttempts, err })
	} else {
		result.Scenarios = append(result.Scenarios, segmentScenario{Name: "sealed_summary", Error: "seal API unavailable"})
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	output := os.Getenv("CPAUK_SEGMENT_BENCH_OUTPUT")
	if output == "" {
		output = filepath.Join(root, envOrDefault("CPAUK_BENCH_LABEL", "segment")+"-segment-results.json")
	}
	if err := os.WriteFile(output, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("CPAUK_SEGMENT_BENCHMARK_RESULT %s\n", encoded)
}

func segmentBenchmarkEvent(at time.Time, id string) model.Event {
	provider, executor, modelName, endpoint, method, path, url := "provider-0", "benchmark", "model-00", "responses", "POST", "/v1/responses", "https://provider.example/v1/responses"
	status, latency := 200, int64(100)
	key := "0000000000000000000000000000000000000000000000000000000000000001"
	algorithm, auth := model.CredentialIDAlgorithm, "oauth"
	credential := "0000000000000000000000000000000000000000000000000000000000000100"
	return model.Event{SchemaVersion: model.EventSchemaVersion, AttemptID: id, ProxyRequestID: id, RequestIDQuality: model.RequestIDSynthetic, KeyID: key, RequestedAt: at.UTC(), Provider: provider, ExecutorType: executor, Model: modelName, EndpointClass: endpoint, AuthType: &auth, CredentialID: &credential, CredentialIDAlgorithm: &algorithm, Succeeded: true, UpstreamStatusCode: &status, LatencyMS: latency, Generated: true, Tokens: model.TokenUsage{Input: 100, Output: 20, Total: 120, Schema: "normalized-v1", Quality: model.TokenQualityExact}, ClientMethod: &method, ClientPath: &path, UpstreamMethod: &method, UpstreamURL: &url, ProxyStatusCode: &status}
}
