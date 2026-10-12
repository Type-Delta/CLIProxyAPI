package store

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

const analyticsBenchmarkSeed = uint64(20261007)

var analyticsBenchmarkEnd = time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)

// TestAnalyticsBenchmarkSeed writes a reproducible 90-day dataset through the real intake path.
// Opt in with CPAUK_BENCH_DIR; CPAUK_BENCH_REQUESTS defaults to 100000. Run this once before
// BenchmarkAnalyticsOperations. Reopening clears SQLite caches, but leaves the OS page cache warm.
func TestAnalyticsBenchmarkSeed(t *testing.T) {
	root := os.Getenv("CPAUK_BENCH_DIR")
	if root == "" {
		t.Skip("set CPAUK_BENCH_DIR to create a benchmark dataset")
	}
	count := 100000
	if value := os.Getenv("CPAUK_BENCH_REQUESTS"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 100000 {
			t.Fatal("CPAUK_BENCH_REQUESTS must be >=100000")
		}
		count = parsed
	}
	if _, err := os.Stat(filepath.Join(root, "analytics.db")); err == nil {
		t.Fatal("benchmark database already exists")
	}
	database := analyticsBenchmarkOpen(t, root)
	defer func() { _ = database.Close(context.Background()) }()
	random := rand.New(rand.NewPCG(analyticsBenchmarkSeed, 31))
	start := analyticsBenchmarkEnd.Add(-90 * 24 * time.Hour)
	batch := make([]model.Event, 0, 500)
	var attempts, retries, failures int64
	elapsed := time.Now()
	write := func() {
		if err := database.WriteBatch(context.Background(), batch); err != nil {
			t.Fatal(err)
		}
		batch = batch[:0]
	}
	for index := 0; index < count; index++ {
		at := start.Add((90 * 24 * time.Hour / time.Duration(count)) * time.Duration(index))
		request := fmt.Sprintf("%032x", index+1)
		modelIndex := random.IntN(12)
		if random.IntN(100) < 70 {
			modelIndex = random.IntN(4)
		}
		key := fmt.Sprintf("%064x", random.IntN(40)+1)
		credential := fmt.Sprintf("%064x", modelIndex%3*10+random.IntN(10)+100)
		input := int64(100 + random.IntN(32000))
		output := int64(20 + random.IntN(6000))
		cacheRead := input * int64(random.IntN(80)) / 100
		cacheCreation := int64(0)
		if random.IntN(10) == 0 {
			cacheCreation = (input - cacheRead) / 2
		}
		attemptCount := 1
		if random.IntN(100) < 9 {
			attemptCount = 2
			retries++
		}
		for attempt := 0; attempt < attemptCount; attempt++ {
			attempts++
			status := 200
			succeeded := true
			var errorClass, errorBody *string
			if attempt < attemptCount-1 || random.IntN(100) < 3 {
				status = 429
				succeeded = false
				failures++
				value := "rate_limit"
				errorClass = &value
				body := `{"error":"quota exceeded","detail":"` + strings.Repeat("e", 1024) + `"}`
				errorBody = &body
			}
			ack := int64(40 + random.IntN(300))
			first := ack + int64(100+random.IntN(2500))
			generation := int64(100 + random.IntN(18000))
			latency := first + generation
			routing := int64(1 + random.IntN(20))
			auth := "oauth"
			algorithm := model.CredentialIDAlgorithm
			tier := "default"
			method := "POST"
			path := "/v1/responses"
			url := "https://provider.example/v1/responses"
			received := at
			sent := at.Add(time.Duration(routing) * time.Millisecond)
			responded := at.Add(time.Duration(latency) * time.Millisecond)
			raw := model.RawJSON(fmt.Sprintf(`{"usage":{"input_tokens":%d,"output_tokens":%d},"diagnostics":"%s"}`, input, output, strings.Repeat("d", 768+random.IntN(512))))
			tokens := model.TokenUsage{Input: input, Output: output, Reasoning: output / 3, Cached: cacheRead, CacheRead: cacheRead, CacheCreation: cacheCreation, Total: input + output, Schema: "normalized-v1", Quality: model.TokenQualityExact}
			if !succeeded {
				tokens = model.TokenUsage{Schema: "normalized-v1", Quality: model.TokenQualityMissing}
				generation = 0
			}
			event := model.Event{SchemaVersion: model.EventSchemaVersion, AttemptID: fmt.Sprintf("%032x", attempts), ProxyRequestID: request, RequestIDQuality: model.RequestIDObserved, KeyID: key, RequestedAt: at.Add(time.Duration(attempt) * time.Millisecond), Provider: fmt.Sprintf("provider-%d", modelIndex%3), ExecutorType: "benchmark", Model: fmt.Sprintf("model-%02d", modelIndex), EndpointClass: "responses", AuthType: &auth, CredentialID: &credential, CredentialIDAlgorithm: &algorithm, Succeeded: succeeded, UpstreamStatusCode: &status, ErrorClass: errorClass, LatencyMS: latency, TimeToFirstTokenMS: &first, ProviderLatencyMS: &ack, FirstTokenLatencyMS: &first, GenerationTimeMS: &generation, RoutingTimeMS: &routing, ServiceTierRequested: &tier, ServiceTierUsed: &tier, Generated: succeeded, Tokens: tokens, ClientMethod: &method, ClientPath: &path, UpstreamMethod: &method, UpstreamURL: &url, ReceivedAt: &received, UpstreamSentAt: &sent, RespondedAt: &responded, UpstreamUsageRaw: &raw, UpstreamErrorBody: errorBody, ProxyStatusCode: &status}
			batch = append(batch, event)
			if len(batch) == cap(batch) {
				write()
			}
		}
	}
	if len(batch) > 0 {
		write()
	}
	duration := time.Since(elapsed)
	if err := database.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(filepath.Join(root, "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("seed=%d requests=%d attempts=%d retries=%d failures=%d ingest_ms=%.3f attempts_per_second=%.2f database_bytes=%d bytes_per_attempt=%.2f schema=%d", analyticsBenchmarkSeed, count, attempts, retries, failures, float64(duration)/float64(time.Millisecond), float64(attempts)/duration.Seconds(), stat.Size(), float64(stat.Size())/float64(attempts), database.SchemaVersion())
}

func analyticsBenchmarkOpen(t testing.TB, root string) *SQLiteStore {
	t.Helper()
	rules := make([]aggregate.PricingRule, 12)
	for index := range rules {
		input := model.NanoUSD(1_000_000_000 + index*100_000_000)
		output := input * 3
		rules[index] = aggregate.PricingRule{ID: fmt.Sprintf("price-%02d", index), Provider: fmt.Sprintf("provider-%d", index%3), Model: fmt.Sprintf("model-%02d", index), InputPerMillion: &input, OutputPerMillion: &output, CacheReadMultiplier: "0.1", CacheCreationMultiplier: "1.25", Source: "benchmark"}
	}
	database, err := Open(context.Background(), Config{Path: filepath.Join(root, "analytics.db"), MaxStorageBytes: 8 << 30, PriceBook: aggregate.PriceBook{Rules: rules}})
	if err != nil {
		t.Fatal(err)
	}
	return database
}

// BenchmarkAnalyticsOperations measures both primed SQLite caches and fresh connections.
// Use -benchtime=3x -count=1 to bound slow baseline analysis queries. Both modes have warm OS
// filesystem caches; "cold_sqlite" excludes connection-open time from the operation measurement.
func BenchmarkAnalyticsOperations(b *testing.B) {
	root := os.Getenv("CPAUK_BENCH_DIR")
	if root == "" {
		b.Skip("set CPAUK_BENCH_DIR to an existing benchmark dataset")
	}
	if _, err := os.Stat(filepath.Join(root, "analytics.db")); err != nil {
		b.Fatal(err)
	}
	type operation struct {
		name      string
		operation model.Operation
		configure func(*model.Query)
		run       func(*SQLiteStore, model.Query) error
	}
	operations := []operation{
		{"summary", model.OperationSummary, nil, func(s *SQLiteStore, q model.Query) error { _, err := s.Summary(context.Background(), q); return err }},
		{"timeseries", model.OperationTimeseries, func(q *model.Query) { q.BucketWidth = "1h" }, func(s *SQLiteStore, q model.Query) error { _, err := s.Timeseries(context.Background(), q); return err }},
		{"timeseries_15m", model.OperationTimeseries, func(q *model.Query) { q.BucketWidth = "15m" }, func(s *SQLiteStore, q model.Query) error { _, err := s.Timeseries(context.Background(), q); return err }},
		{"dimensions", model.OperationDimensions, func(q *model.Query) { q.Dimension = "model"; q.PageSize = 20 }, func(s *SQLiteStore, q model.Query) error { _, err := s.Dimensions(context.Background(), q); return err }},
		{"events", model.OperationEvents, func(q *model.Query) { q.PageSize = 50 }, func(s *SQLiteStore, q model.Query) error { _, err := s.Events(context.Background(), q); return err }},
		{"leaderboard", model.OperationLeaderboard, func(q *model.Query) { q.SortBy = model.LeaderboardSortTokens; q.PageSize = 20 }, func(s *SQLiteStore, q model.Query) error {
			_, err := s.Leaderboard(context.Background(), q)
			return err
		}},
		{"activity", model.OperationActivity, func(q *model.Query) { q.Window = "month" }, func(s *SQLiteStore, q model.Query) error { _, err := s.Activity(context.Background(), q); return err }},
		{"analysis", model.OperationAnalysis, func(q *model.Query) { q.BucketWidth = "1h" }, func(s *SQLiteStore, q model.Query) error { _, err := s.Analysis(context.Background(), q); return err }},
		{"analysis_costs", model.OperationAnalysis, nil, func(s *SQLiteStore, q model.Query) error {
			_, err := s.analysisCosts(context.Background(), q)
			return err
		}},
		{"analysis_latency", model.OperationAnalysis, nil, func(s *SQLiteStore, q model.Query) error {
			_, err := s.analysisLatency(context.Background(), q)
			return err
		}},
		{"analysis_models", model.OperationAnalysis, func(q *model.Query) { q.BucketWidth = "1h" }, func(s *SQLiteStore, q model.Query) error {
			_, err := s.analysisModels(context.Background(), q)
			return err
		}},
	}
	for _, days := range []int{7, 30, 90} {
		for _, op := range operations {
			for _, mode := range []string{"warm", "cold_sqlite"} {
				b.Run(fmt.Sprintf("%dd/%s/%s", days, op.name, mode), func(b *testing.B) {
					q := model.Query{SchemaVersion: 2, Operation: op.operation, Start: analyticsBenchmarkEnd.Add(-time.Duration(days) * 24 * time.Hour), End: analyticsBenchmarkEnd, TimeZone: "UTC"}
					if op.configure != nil {
						op.configure(&q)
					}
					database := analyticsBenchmarkOpen(b, root)
					defer func() { _ = database.Close(context.Background()) }()
					if err := op.run(database, q); err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					b.ResetTimer()
					for index := 0; index < b.N; index++ {
						if mode == "cold_sqlite" {
							b.StopTimer()
							_ = database.Close(context.Background())
							database = analyticsBenchmarkOpen(b, root)
							b.StartTimer()
						}
						if err := op.run(database, q); err != nil {
							b.Fatal(err)
						}
					}
					b.StopTimer()
					b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "queries/s")
				})
			}
		}
	}
}
