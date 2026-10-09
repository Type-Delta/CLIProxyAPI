package store

// This file is an opt-in, end-to-end benchmark for the CPAUK storage boundary.
// It intentionally lives in the store package so it can time the individual
// analysis sections (costs, latency, and models) as well as the public query
// methods. The benchmark does not run during ordinary `go test ./...`.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

const (
	compactBenchmarkEnabled = "CPAUK_COMPACT_BENCHMARK"
	compactBenchmarkDir     = "CPAUK_BENCH_DIR"
	compactBenchmarkOutput  = "CPAUK_BENCH_OUTPUT"
	compactBenchmarkReport  = "CPAUK_BENCH_REPORT"
	compactBenchmarkCounts  = "CPAUK_BENCH_REQUESTS"
	compactBenchmarkRepeat  = "CPAUK_BENCH_REPETITIONS"
	compactBenchmarkRebuild = "CPAUK_BENCH_REBUILD"
	compactBenchmarkCommand = "CPAUK_BENCH_COMMAND"
	compactBenchmarkOps     = "CPAUK_BENCH_OPERATIONS"
	compactBenchmarkWindows = "CPAUK_BENCH_WINDOWS"
	compactBenchmarkSeed    = uint64(20261007)
)

var compactBenchmarkEnd = time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)

// TestCPAUKCompactBenchmark creates deterministic request histories and then
// runs the same storage operations against each requested history. Set
// CPAUK_COMPACT_BENCHMARK=1 to opt in. The wrapper in test/perf sets all paths
// and writes the JSON and Markdown artifacts.
func TestCPAUKCompactBenchmark(t *testing.T) {
	if os.Getenv(compactBenchmarkEnabled) != "1" {
		t.Skipf("set %s=1 to run the CPAUK compact benchmark", compactBenchmarkEnabled)
	}
	root := os.Getenv(compactBenchmarkDir)
	if root == "" {
		t.Fatal("CPAUK_BENCH_DIR is required when the compact benchmark is enabled")
	}
	counts, err := compactBenchmarkCountsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	repetitions := 1
	if value := os.Getenv(compactBenchmarkRepeat); value != "" {
		repetitions, err = strconv.Atoi(value)
		if err != nil || repetitions < 1 || repetitions > 20 {
			t.Fatalf("%s must be an integer from 1 to 20", compactBenchmarkRepeat)
		}
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create benchmark root: %v", err)
	}

	run := compactBenchmarkResult{
		Schema:      1,
		Label:       envOrDefault("CPAUK_BENCH_LABEL", "baseline"),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Command:     envOrDefault(compactBenchmarkCommand, "go test -run '^TestCPAUKCompactBenchmark$' ./internal/cpauk/store"),
		GitRevision: compactBenchmarkGitRevision(),
		Generator:   compactBenchmarkGenerator{Seed: compactBenchmarkSeed, End: compactBenchmarkEnd},
		Machine:     compactBenchmarkMachineMetadata(),
		Repetitions: repetitions,
		Runs:        make([]compactBenchmarkDataset, 0, len(counts)),
	}
	for _, count := range counts {
		dataset, errRun := compactBenchmarkDatasetRun(t, root, count, repetitions)
		if errRun != nil {
			t.Fatal(errRun)
		}
		run.Runs = append(run.Runs, dataset)
	}
	if err := compactBenchmarkWriteArtifacts(run); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("CPAUK_COMPACT_BENCHMARK_RESULT %s\n", encoded)
}

type compactBenchmarkResult struct {
	Schema      int                       `json:"schema"`
	Label       string                    `json:"label"`
	GeneratedAt string                    `json:"generated_at"`
	Command     string                    `json:"command"`
	GitRevision string                    `json:"git_revision,omitempty"`
	Generator   compactBenchmarkGenerator `json:"generator"`
	Machine     compactBenchmarkMachine   `json:"machine"`
	Repetitions int                       `json:"repetitions"`
	Runs        []compactBenchmarkDataset `json:"runs"`
}

type compactBenchmarkGenerator struct {
	Seed uint64    `json:"seed"`
	End  time.Time `json:"end"`
}

type compactBenchmarkDataset struct {
	Requests          int                     `json:"requests"`
	Attempts          int                     `json:"attempts"`
	Retries           int                     `json:"retries"`
	Failures          int                     `json:"failures"`
	GeneratorNS       int64                   `json:"generator_ns"`
	WriteNS           int64                   `json:"write_ns"`
	IngestNS          int64                   `json:"ingest_ns"`
	AttemptsPerSecond float64                 `json:"attempts_per_second"`
	DatabaseBytes     int64                   `json:"database_bytes"`
	DatabaseFiles     map[string]int64        `json:"database_files"`
	Rows              map[string]int64        `json:"rows"`
	SchemaVersion     int                     `json:"schema_version"`
	QueryResults      []compactBenchmarkQuery `json:"queries"`
	DatasetPath       string                  `json:"dataset_path"`
	IngestError       string                  `json:"ingest_error,omitempty"`
}

type compactBenchmarkSeedMetadata struct {
	Attempts          int     `json:"attempts"`
	Retries           int     `json:"retries"`
	Failures          int     `json:"failures"`
	GeneratorNS       int64   `json:"generator_ns"`
	WriteNS           int64   `json:"write_ns"`
	IngestNS          int64   `json:"ingest_ns"`
	AttemptsPerSecond float64 `json:"attempts_per_second"`
}

type compactBenchmarkQuery struct {
	Name         string  `json:"name"`
	Window       string  `json:"window"`
	WarmMedianNS int64   `json:"warm_median_ns"`
	WarmP95NS    int64   `json:"warm_p95_ns"`
	ColdMedianNS int64   `json:"cold_median_ns"`
	ColdP95NS    int64   `json:"cold_p95_ns"`
	ColdOpenNS   int64   `json:"cold_open_median_ns"`
	ColdTotalNS  int64   `json:"cold_total_median_ns"`
	WarmSamples  []int64 `json:"warm_samples_ns"`
	ColdSamples  []int64 `json:"cold_samples_ns"`
	ColdOpen     []int64 `json:"cold_open_samples_ns"`
	ColdTotals   []int64 `json:"cold_total_samples_ns"`
	OutputCount  int     `json:"output_count"`
	Error        string  `json:"error,omitempty"`
}

type compactBenchmarkMachine struct {
	Hostname     string `json:"hostname,omitempty"`
	OS           string `json:"os"`
	Kernel       string `json:"kernel,omitempty"`
	Architecture string `json:"architecture"`
	GoVersion    string `json:"go_version"`
	LogicalCPUs  int    `json:"logical_cpus"`
	CPUModel     string `json:"cpu_model,omitempty"`
	MemoryBytes  uint64 `json:"memory_bytes,omitempty"`
	RunnerClass  string `json:"runner_class,omitempty"`
}

func compactBenchmarkDatasetRun(t testing.TB, root string, requestCount, repetitions int) (compactBenchmarkDataset, error) {
	t.Helper()
	datasetRoot := filepath.Join(root, fmt.Sprintf("requests-%d", requestCount))
	databasePath := filepath.Join(datasetRoot, "analytics.db")
	if os.Getenv(compactBenchmarkRebuild) == "1" {
		if err := os.RemoveAll(datasetRoot); err != nil {
			return compactBenchmarkDataset{}, fmt.Errorf("remove benchmark dataset %s: %w", datasetRoot, err)
		}
	}
	if _, err := os.Stat(databasePath); err == nil {
		return compactBenchmarkQueryDataset(t, datasetRoot, requestCount, repetitions)
	} else if !os.IsNotExist(err) {
		return compactBenchmarkDataset{}, fmt.Errorf("inspect benchmark dataset %s: %w", databasePath, err)
	}
	if err := os.MkdirAll(datasetRoot, 0o700); err != nil {
		return compactBenchmarkDataset{}, fmt.Errorf("create benchmark dataset %s: %w", datasetRoot, err)
	}

	database, err := compactBenchmarkOpen(context.Background(), datasetRoot)
	if err != nil {
		return compactBenchmarkDataset{}, err
	}
	defer func() { _ = database.Close(context.Background()) }()

	result := compactBenchmarkDataset{Requests: requestCount, DatasetPath: datasetRoot}
	batch := make([]model.Event, 0, 500)
	random := rand.New(rand.NewPCG(compactBenchmarkSeed, 31))
	start := compactBenchmarkEnd.Add(-90 * 24 * time.Hour)
	step := (90 * 24 * time.Hour) / time.Duration(requestCount)
	var attempts, retries, failures int
	var generatorNS, writeNS int64
	ingestStarted := time.Now()
	writeBatch := func() error {
		if len(batch) == 0 {
			return nil
		}
		started := time.Now()
		if err := database.WriteBatch(context.Background(), batch); err != nil {
			return err
		}
		writeNS += time.Since(started).Nanoseconds()
		batch = batch[:0]
		return nil
	}
	for index := 0; index < requestCount; index++ {
		generationStarted := time.Now()
		at := start.Add(step * time.Duration(index))
		requestID := fmt.Sprintf("%032x", index+1)
		modelIndex := random.IntN(12)
		if random.IntN(100) < 70 {
			modelIndex = random.IntN(4)
		}
		keyID := fmt.Sprintf("%064x", random.IntN(40)+1)
		credentialID := fmt.Sprintf("%064x", modelIndex%3*10+random.IntN(10)+100)
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
			event := model.Event{
				SchemaVersion: model.EventSchemaVersion, AttemptID: fmt.Sprintf("%032x", attempts), ProxyRequestID: requestID,
				RequestIDQuality: model.RequestIDObserved, KeyID: keyID, RequestedAt: at.Add(time.Duration(attempt) * time.Millisecond),
				Provider: fmt.Sprintf("provider-%d", modelIndex%3), ExecutorType: "benchmark", Model: fmt.Sprintf("model-%02d", modelIndex),
				EndpointClass: "responses", AuthType: &auth, CredentialID: &credentialID, CredentialIDAlgorithm: &algorithm,
				Succeeded: succeeded, UpstreamStatusCode: &status, ErrorClass: errorClass, LatencyMS: latency, TimeToFirstTokenMS: &first,
				ProviderLatencyMS: &ack, FirstTokenLatencyMS: &first, GenerationTimeMS: &generation, RoutingTimeMS: &routing,
				ServiceTierRequested: &tier, ServiceTierUsed: &tier, Generated: succeeded, Tokens: tokens,
				ClientMethod: &method, ClientPath: &path, UpstreamMethod: &method, UpstreamURL: &url, ReceivedAt: &received,
				UpstreamSentAt: &sent, RespondedAt: &responded, UpstreamUsageRaw: &raw, UpstreamErrorBody: errorBody,
				ProxyStatusCode: &status,
			}
			batch = append(batch, event)
		}
		generatorNS += time.Since(generationStarted).Nanoseconds()
		if len(batch) == cap(batch) {
			if err := writeBatch(); err != nil {
				return compactBenchmarkDataset{}, fmt.Errorf("ingest batch at request %d: %w", index, err)
			}
		}
	}
	if err := writeBatch(); err != nil {
		return compactBenchmarkDataset{}, fmt.Errorf("ingest final batch: %w", err)
	}
	if err := database.Checkpoint(context.Background()); err != nil {
		return compactBenchmarkDataset{}, err
	}
	result.Attempts, result.Retries, result.Failures = attempts, retries, failures
	result.GeneratorNS, result.WriteNS, result.IngestNS = generatorNS, writeNS, time.Since(ingestStarted).Nanoseconds()
	if result.IngestNS > 0 {
		result.AttemptsPerSecond = float64(attempts) / (float64(result.IngestNS) / float64(time.Second))
	}
	result.SchemaVersion = database.SchemaVersion()
	result.Rows, err = compactBenchmarkRowCounts(context.Background(), database)
	if err != nil {
		return compactBenchmarkDataset{}, err
	}
	result.DatabaseFiles = compactBenchmarkDatabaseFiles(datasetRoot)
	result.DatabaseBytes = compactBenchmarkDatabaseBytes(result.DatabaseFiles)
	metadata := compactBenchmarkSeedMetadata{Attempts: result.Attempts, Retries: result.Retries, Failures: result.Failures,
		GeneratorNS: result.GeneratorNS, WriteNS: result.WriteNS, IngestNS: result.IngestNS, AttemptsPerSecond: result.AttemptsPerSecond}
	metadataJSON, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return compactBenchmarkDataset{}, fmt.Errorf("encode seed metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(datasetRoot, "seed-metadata.json"), append(metadataJSON, '\n'), 0o600); err != nil {
		return compactBenchmarkDataset{}, fmt.Errorf("write seed metadata: %w", err)
	}
	result.QueryResults, err = compactBenchmarkQueries(t, datasetRoot, requestCount, repetitions)
	if err != nil {
		return compactBenchmarkDataset{}, err
	}
	return result, nil
}

func compactBenchmarkQueryDataset(t testing.TB, datasetRoot string, requestCount, repetitions int) (compactBenchmarkDataset, error) {
	database, err := compactBenchmarkOpen(context.Background(), datasetRoot)
	if err != nil {
		return compactBenchmarkDataset{}, err
	}
	defer func() { _ = database.Close(context.Background()) }()
	rows, err := compactBenchmarkRowCounts(context.Background(), database)
	if err != nil {
		return compactBenchmarkDataset{}, err
	}
	files := compactBenchmarkDatabaseFiles(datasetRoot)
	result := compactBenchmarkDataset{
		Requests: requestCount, Attempts: int(rows["events"]), DatasetPath: datasetRoot,
		Rows: rows, DatabaseFiles: files, DatabaseBytes: compactBenchmarkDatabaseBytes(files), SchemaVersion: database.SchemaVersion(),
	}
	var metadata compactBenchmarkSeedMetadata
	if data, readErr := os.ReadFile(filepath.Join(datasetRoot, "seed-metadata.json")); readErr == nil {
		if decodeErr := json.Unmarshal(data, &metadata); decodeErr != nil {
			return compactBenchmarkDataset{}, fmt.Errorf("decode seed metadata: %w", decodeErr)
		}
		result.Attempts, result.Retries, result.Failures = metadata.Attempts, metadata.Retries, metadata.Failures
		result.GeneratorNS, result.WriteNS, result.IngestNS, result.AttemptsPerSecond = metadata.GeneratorNS, metadata.WriteNS, metadata.IngestNS, metadata.AttemptsPerSecond
	} else if !os.IsNotExist(readErr) {
		return compactBenchmarkDataset{}, fmt.Errorf("read seed metadata: %w", readErr)
	}
	result.QueryResults, err = compactBenchmarkQueries(t, datasetRoot, requestCount, repetitions)
	return result, err
}

func compactBenchmarkOpen(ctx context.Context, root string) (*SQLiteStore, error) {
	rules := make([]aggregate.PricingRule, 12)
	for index := range rules {
		input := model.NanoUSD(1_000_000_000 + index*100_000_000)
		output := input * 3
		rules[index] = aggregate.PricingRule{ID: fmt.Sprintf("price-%02d", index), Provider: fmt.Sprintf("provider-%d", index%3), Model: fmt.Sprintf("model-%02d", index), InputPerMillion: &input, OutputPerMillion: &output, CacheReadMultiplier: "0.1", CacheCreationMultiplier: "1.25", Source: "benchmark"}
	}
	return Open(ctx, Config{Path: filepath.Join(root, "analytics.db"), MaxStorageBytes: 8 << 30, PriceBook: aggregate.PriceBook{Rules: rules}})
}

func compactBenchmarkQueries(t testing.TB, datasetRoot string, requestCount, repetitions int) ([]compactBenchmarkQuery, error) {
	t.Helper()
	results := make([]compactBenchmarkQuery, 0, 44)
	allowedWindows := compactBenchmarkFilterSet(compactBenchmarkWindows)
	allowedOperations := compactBenchmarkFilterSet(compactBenchmarkOps)
	for _, window := range []struct {
		name string
		dur  time.Duration
	}{
		{name: "1h", dur: time.Hour}, {name: "6h", dur: 6 * time.Hour}, {name: "30d", dur: 30 * 24 * time.Hour}, {name: "90d", dur: 90 * 24 * time.Hour},
	} {
		if len(allowedWindows) > 0 {
			if _, ok := allowedWindows[window.name]; !ok {
				continue
			}
		}
		start := compactBenchmarkEnd.Add(-window.dur)
		base := func(operation model.Operation) model.Query {
			return model.Query{SchemaVersion: model.QuerySchemaVersionV2, Operation: operation, Start: start, End: compactBenchmarkEnd, TimeZone: "UTC"}
		}
		operations := []compactBenchmarkOperation{
			{Name: "summary", Query: base(model.OperationSummary), Run: func(s *SQLiteStore, q model.Query) (int, error) {
				_, err := s.Summary(context.Background(), q)
				return 1, err
			}},
			{Name: "activity", Query: func() model.Query {
				q := base(model.OperationActivity)
				q.Window = compactBenchmarkActivityWindow(window.name)
				return q
			}(), Run: func(s *SQLiteStore, q model.Query) (int, error) {
				result, err := s.Activity(context.Background(), q)
				return len(result.Buckets), err
			}},
			{Name: "analysis", Query: compactBenchmarkAnalysisQuery(base(model.OperationAnalysis), window.name), Run: func(s *SQLiteStore, q model.Query) (int, error) {
				result, err := s.Analysis(context.Background(), q)
				if result.SeriesByCategory == nil {
					return 0, err
				}
				return len(result.SeriesByCategory.Buckets), err
			}},
			{Name: "costs", Query: base(model.OperationAnalysis), Run: func(s *SQLiteStore, q model.Query) (int, error) {
				result, err := s.analysisCosts(context.Background(), q)
				return len(result.Models), err
			}},
			{Name: "latency", Query: base(model.OperationAnalysis), Run: func(s *SQLiteStore, q model.Query) (int, error) {
				result, err := s.analysisLatency(context.Background(), q)
				return len(result.Samples), err
			}},
			{Name: "models", Query: compactBenchmarkAnalysisQuery(base(model.OperationAnalysis), window.name), Run: func(s *SQLiteStore, q model.Query) (int, error) {
				result, err := s.analysisModels(context.Background(), q)
				return len(result.Models) + len(result.Buckets), err
			}},
			{Name: "dimensions", Query: func() model.Query {
				q := base(model.OperationDimensions)
				q.Dimension = "model"
				q.PageSize = 20
				return q
			}(), Run: func(s *SQLiteStore, q model.Query) (int, error) {
				result, err := s.Dimensions(context.Background(), q)
				return len(result.Rows), err
			}},
			{Name: "leaderboard", Query: func() model.Query {
				q := base(model.OperationLeaderboard)
				q.SortBy = model.LeaderboardSortTokens
				q.PageSize = 20
				return q
			}(), Run: func(s *SQLiteStore, q model.Query) (int, error) {
				result, err := s.Leaderboard(context.Background(), q)
				return len(result.Rows), err
			}},
		}
		for _, width := range []string{"15m", "30m", "1h"} {
			q := base(model.OperationTimeseries)
			q.BucketWidth = width
			operations = append(operations, compactBenchmarkOperation{Name: "timeseries_" + width, Query: q, Run: func(s *SQLiteStore, q model.Query) (int, error) {
				result, err := s.Timeseries(context.Background(), q)
				return len(result.Points), err
			}})
		}
		for _, operation := range operations {
			if len(allowedOperations) > 0 {
				if _, ok := allowedOperations[operation.Name]; !ok {
					continue
				}
			}
			result := compactBenchmarkQuery{Name: operation.Name, Window: window.name}
			warm, cold, opens, totals, outputCount, errRun := compactBenchmarkMeasure(t, datasetRoot, operation, repetitions)
			result.WarmSamples, result.ColdSamples, result.ColdOpen, result.ColdTotals = warm, cold, opens, totals
			result.WarmMedianNS, result.WarmP95NS = compactBenchmarkMedian(warm), compactBenchmarkP95(warm)
			result.ColdMedianNS, result.ColdP95NS = compactBenchmarkMedian(cold), compactBenchmarkP95(cold)
			result.ColdOpenNS, result.ColdTotalNS = compactBenchmarkMedian(opens), compactBenchmarkMedian(totals)
			result.OutputCount = outputCount
			if errRun != nil {
				result.Error = errRun.Error()
			}
			results = append(results, result)
		}
	}
	_ = requestCount
	return results, nil
}

type compactBenchmarkOperation struct {
	Name  string
	Query model.Query
	Run   func(*SQLiteStore, model.Query) (int, error)
}

func compactBenchmarkMeasure(t testing.TB, datasetRoot string, operation compactBenchmarkOperation, repetitions int) ([]int64, []int64, []int64, []int64, int, error) {
	t.Helper()
	database, err := compactBenchmarkOpen(context.Background(), datasetRoot)
	if err != nil {
		return nil, nil, nil, nil, 0, err
	}
	defer func() { _ = database.Close(context.Background()) }()
	if _, err := operation.Run(database, operation.Query); err != nil {
		return nil, nil, nil, nil, 0, fmt.Errorf("prime %s: %w", operation.Name, err)
	}
	warm := make([]int64, 0, repetitions)
	outputCount := 0
	for index := 0; index < repetitions; index++ {
		started := time.Now()
		outputCount, err = operation.Run(database, operation.Query)
		warm = append(warm, time.Since(started).Nanoseconds())
		if err != nil {
			return warm, nil, nil, nil, outputCount, fmt.Errorf("warm %s: %w", operation.Name, err)
		}
	}
	cold := make([]int64, 0, repetitions)
	opens := make([]int64, 0, repetitions)
	totals := make([]int64, 0, repetitions)
	for index := 0; index < repetitions; index++ {
		if err := database.Close(context.Background()); err != nil {
			return warm, cold, opens, totals, outputCount, fmt.Errorf("close before cold %s: %w", operation.Name, err)
		}
		openStarted := time.Now()
		database, err = compactBenchmarkOpen(context.Background(), datasetRoot)
		openDuration := time.Since(openStarted).Nanoseconds()
		if err != nil {
			return warm, cold, opens, totals, outputCount, fmt.Errorf("open cold %s: %w", operation.Name, err)
		}
		queryStarted := time.Now()
		outputCount, err = operation.Run(database, operation.Query)
		queryDuration := time.Since(queryStarted).Nanoseconds()
		opens = append(opens, openDuration)
		cold = append(cold, queryDuration)
		totals = append(totals, openDuration+queryDuration)
		if err != nil {
			return warm, cold, opens, totals, outputCount, fmt.Errorf("cold %s: %w", operation.Name, err)
		}
	}
	return warm, cold, opens, totals, outputCount, nil
}

func compactBenchmarkActivityWindow(name string) string {
	if name == "90d" {
		return "year"
	}
	if name == "30d" {
		return "month"
	}
	return "day"
}

func compactBenchmarkAnalysisQuery(query model.Query, window string) model.Query {
	switch window {
	case "1h":
		query.BucketWidth = "15m"
	case "6h":
		query.BucketWidth = "30m"
	case "30d":
		query.BucketWidth = "1h"
	default:
		query.BucketWidth = "1d"
	}
	return query
}

func compactBenchmarkRowCounts(ctx context.Context, database *SQLiteStore) (map[string]int64, error) {
	database.mu.RLock()
	defer database.mu.RUnlock()
	if database.db == nil {
		return nil, ErrClosed
	}
	rows, err := database.db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make(map[string]int64, len(tables))
	for _, name := range tables {
		var count int64
		if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+strings.ReplaceAll(name, `"`, `""`)+`"`).Scan(&count); err != nil {
			return nil, fmt.Errorf("count table %s: %w", name, err)
		}
		result[name] = count
	}
	return result, nil
}

func compactBenchmarkDatabaseFiles(root string) map[string]int64 {
	result := map[string]int64{}
	for _, name := range []string{"analytics.db", "analytics.db-wal", "analytics.db-shm"} {
		stat, err := os.Stat(filepath.Join(root, name))
		if err == nil {
			result[name] = stat.Size()
		}
	}
	return result
}

func compactBenchmarkDatabaseBytes(files map[string]int64) int64 {
	var total int64
	for _, size := range files {
		total += size
	}
	return total
}

func compactBenchmarkCountsFromEnv() ([]int, error) {
	value := os.Getenv(compactBenchmarkCounts)
	if value == "" {
		return []int{100000}, nil
	}
	parts := strings.Split(value, ",")
	result := make([]int, 0, len(parts))
	seen := map[int]struct{}{}
	for _, part := range parts {
		count, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || count < 100000 || count > 5_000_000 {
			return nil, fmt.Errorf("%s must contain request counts from 100000 to 5000000", compactBenchmarkCounts)
		}
		if _, ok := seen[count]; ok {
			continue
		}
		seen[count] = struct{}{}
		result = append(result, count)
	}
	slices.Sort(result)
	return result, nil
}

func compactBenchmarkMedian(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[(len(sorted)-1)/2]
}

func compactBenchmarkP95(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := (95*len(sorted)+99)/100 - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func compactBenchmarkMachineMetadata() compactBenchmarkMachine {
	result := compactBenchmarkMachine{OS: runtime.GOOS, Architecture: runtime.GOARCH, GoVersion: runtime.Version(), LogicalCPUs: runtime.NumCPU(), RunnerClass: os.Getenv("CPA_PERF_RUNNER_CLASS")}
	result.Hostname, _ = os.Hostname()
	if output, err := exec.Command("uname", "-sr").Output(); err == nil {
		result.Kernel = strings.TrimSpace(string(output))
	}
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				result.OS = strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
				break
			}
		}
	}
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.ToLower(line), "model name") {
				if index := strings.IndexByte(line, ':'); index >= 0 {
					result.CPUModel = strings.TrimSpace(line[index+1:])
				}
				break
			}
		}
	}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "MemTotal:" {
				if kib, parseErr := strconv.ParseUint(fields[1], 10, 64); parseErr == nil {
					result.MemoryBytes = kib * 1024
				}
				break
			}
		}
	}
	return result
}

func compactBenchmarkGitRevision() string {
	command := exec.Command("git", "rev-parse", "HEAD")
	if output, err := command.Output(); err == nil {
		return strings.TrimSpace(string(output))
	}
	return ""
}

func compactBenchmarkWriteArtifacts(result compactBenchmarkResult) error {
	output := os.Getenv(compactBenchmarkOutput)
	if output == "" {
		output = filepath.Join(os.Getenv(compactBenchmarkDir), "results.json")
	}
	report := os.Getenv(compactBenchmarkReport)
	if report == "" {
		report = filepath.Join(filepath.Dir(output), "report.md")
	}
	for _, path := range []string{output, report} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create artifact directory: %w", err)
		}
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(output, data, 0o600); err != nil {
		return fmt.Errorf("write benchmark JSON: %w", err)
	}
	markdown := compactBenchmarkMarkdown(result)
	if err := os.WriteFile(report, []byte(markdown), 0o600); err != nil {
		return fmt.Errorf("write benchmark Markdown: %w", err)
	}
	return nil
}

func compactBenchmarkMarkdown(result compactBenchmarkResult) string {
	var builder strings.Builder
	builder.WriteString("# CPAUK compact aggregate benchmark\n\n")
	fmt.Fprintf(&builder, "Captured `%s` at revision `%s`.\n\n", result.GeneratedAt, result.GitRevision)
	fmt.Fprintf(&builder, "Command: `%s`\n\n", result.Command)
	builder.WriteString("## Machine\n\n")
	fmt.Fprintf(&builder, "- OS: `%s`\n- Kernel: `%s`\n- Go: `%s`\n- Architecture: `%s`\n- CPUs: `%d`\n- CPU: `%s`\n- Memory bytes: `%d`\n- Host: `%s`\n\n", result.Machine.OS, result.Machine.Kernel, result.Machine.GoVersion, result.Machine.Architecture, result.Machine.LogicalCPUs, result.Machine.CPUModel, result.Machine.MemoryBytes, result.Machine.Hostname)
	fmt.Fprintf(&builder, "Generator seed: `%d`; fixed end: `%s`; repetitions: `%d`.\n\n", result.Generator.Seed, result.Generator.End.Format(time.RFC3339), result.Repetitions)
	builder.WriteString("The cold query column closes and reopens SQLite for each sample, then times only the query. `cold total` includes that reopen. The operating system page cache is intentionally left untouched, so these are fresh SQLite connection measurements rather than a guaranteed cold disk read.\n\n")
	for _, dataset := range result.Runs {
		fmt.Fprintf(&builder, "## %d requests\n\n", dataset.Requests)
		fmt.Fprintf(&builder, "- Dataset: `%s`\n- Attempts: %d (retries %d, failures %d)\n- Ingest: %.3f s (writes %.3f s, generation %.3f s, %.2f attempts/s)\n- Database bytes: %d\n- Schema: %d\n\n", dataset.DatasetPath, dataset.Attempts, dataset.Retries, dataset.Failures, float64(dataset.IngestNS)/float64(time.Second), float64(dataset.WriteNS)/float64(time.Second), float64(dataset.GeneratorNS)/float64(time.Second), dataset.AttemptsPerSecond, dataset.DatabaseBytes, dataset.SchemaVersion)
		builder.WriteString("Rows:\n\n")
		builder.WriteString("| Table | Rows |\n| --- | ---: |\n")
		tableNames := make([]string, 0, len(dataset.Rows))
		for table := range dataset.Rows {
			tableNames = append(tableNames, table)
		}
		slices.Sort(tableNames)
		for _, table := range tableNames {
			fmt.Fprintf(&builder, "| `%s` | %d |\n", table, dataset.Rows[table])
		}
		builder.WriteString("\nQuery latency (nanoseconds; medians, one sample unless repetitions is increased):\n\n")
		builder.WriteString("| Window | Operation | Warm | Warm p95 | Cold query | Cold p95 | Reopen | Cold total | Output | Error |\n| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |\n")
		for _, query := range dataset.QueryResults {
			errText := query.Error
			if errText == "" {
				errText = "-"
			}
			fmt.Fprintf(&builder, "| %s | `%s` | %d | %d | %d | %d | %d | %d | %d | %s |\n", query.Window, query.Name, query.WarmMedianNS, query.WarmP95NS, query.ColdMedianNS, query.ColdP95NS, query.ColdOpenNS, query.ColdTotalNS, query.OutputCount, strings.ReplaceAll(errText, "|", "\\|"))
		}
		builder.WriteString("\n")
	}
	return builder.String()
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func compactBenchmarkFilterSet(name string) map[string]struct{} {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return nil
	}
	result := make(map[string]struct{})
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result[item] = struct{}{}
		}
	}
	return result
}
