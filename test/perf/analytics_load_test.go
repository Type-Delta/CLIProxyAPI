package perf

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const fullProfileEnvironment = "CPA_ANALYTICS_LOAD_PROFILE"

const recordedResultsEnvironment = "CPA_ANALYTICS_LOAD_RESULTS_FILE"

type loadMode string

const (
	modeDisabled            loadMode = "disabled"
	modeHealthy             loadMode = "healthy"
	modeSQLiteBlocked       loadMode = "sqlite_blocked"
	modeBothQueuesSaturated loadMode = "both_queues_saturated"
)

type trafficKind string

const (
	trafficJSON         trafficKind = "json"
	trafficSSE          trafficKind = "sse"
	trafficWebSocket    trafficKind = "websocket"
	trafficFailureRetry trafficKind = "failure_retry"
)

type loadProfile struct {
	Warmup              time.Duration
	Measurement         time.Duration
	Clients             int
	CompletedPerSecond  int
	GoroutineSampleRate time.Duration
	ShutdownTimeout     time.Duration
	Modes               []loadMode
}

var fixedAnalyticsLoadProfile = loadProfile{
	Warmup:              30 * time.Second,
	Measurement:         5 * time.Minute,
	Clients:             64,
	CompletedPerSecond:  1000,
	GoroutineSampleRate: 5 * time.Second,
	ShutdownTimeout:     10 * time.Second,
	Modes: []loadMode{
		modeDisabled,
		modeHealthy,
		modeSQLiteBlocked,
		modeBothQueuesSaturated,
	},
}

type upstreamEndpoints struct {
	JSONURL      string
	SSEURL       string
	WebSocketURL string
	FailureURL   string
}

type analyticsAdapterConfig struct {
	Mode     loadMode
	Upstream upstreamEndpoints
}

type loadRequest struct {
	ID          string
	Kind        trafficKind
	ScheduledAt time.Time
}

type loadResponse struct {
	Kind     trafficKind
	Attempts int
	Success  bool
}

type adapterMetrics struct {
	AnalyticsReady         bool
	SQLiteBlocked          bool
	GenericLaneWorkers     int
	GenericQueueCapacity   int
	GenericQueueDepth      int
	AnalyticsQueueCapacity int
	AnalyticsQueueDepth    int
	GenericQueueDropped    uint64
	AnalyticsQueueDropped  uint64
	QueueWaitCount         uint64
	QueueWaitTotal         time.Duration
	QueueWaitMax           time.Duration
}

// analyticsLoadAdapter is deliberately local to the performance test package.
// A production integration file in this directory installs a factory once the
// CPA analytics and usage-delivery contracts are available.
type analyticsLoadAdapter interface {
	Do(context.Context, loadRequest) (loadResponse, error)
	Metrics() adapterMetrics
	Close(context.Context) error
}

var analyticsLoadAdapterFactory func(analyticsAdapterConfig) (analyticsLoadAdapter, error)

type modeResult struct {
	Mode                          loadMode       `json:"mode"`
	Completed                     int            `json:"completed"`
	ActualCompletedPerSecond      float64        `json:"actual_completed_per_second"`
	P99LatencyNanoseconds         int64          `json:"p99_latency_ns"`
	P99DispatchLagNanoseconds     int64          `json:"p99_dispatch_lag_ns"`
	HeapAfterGCBytes              uint64         `json:"heap_after_gc_bytes"`
	GoroutinesBefore              int            `json:"goroutines_before"`
	GoroutinesMaximum             int            `json:"goroutines_maximum"`
	GoroutinesFinalSlopePerMinute float64        `json:"goroutines_final_slope_per_minute"`
	GoroutinesAfterShutdown       int            `json:"goroutines_after_shutdown"`
	ShutdownNanoseconds           int64          `json:"shutdown_ns"`
	QueueWaitCount                uint64         `json:"queue_wait_count"`
	QueueWaitTotalNanoseconds     int64          `json:"queue_wait_total_ns"`
	QueueWaitMaxNanoseconds       int64          `json:"queue_wait_max_ns"`
	GenericQueueCapacity          int            `json:"generic_queue_capacity"`
	GenericQueueDepth             int            `json:"generic_queue_depth"`
	AnalyticsQueueCapacity        int            `json:"analytics_queue_capacity"`
	AnalyticsQueueDepth           int            `json:"analytics_queue_depth"`
	GenericQueueDropped           uint64         `json:"generic_queue_dropped"`
	AnalyticsQueueDropped         uint64         `json:"analytics_queue_dropped"`
	GenericLaneWorkers            int            `json:"generic_lane_workers"`
	Upstream                      upstreamCounts `json:"upstream"`
	Runner                        runnerMetadata `json:"runner"`
}

type recordedRun struct {
	Results []modeResult   `json:"results"`
	Runner  runnerMetadata `json:"runner"`
}

type aggregateResult struct {
	DisabledMedianP99Nanoseconds int64 `json:"disabled_median_p99_ns"`
	HealthyMedianP99Nanoseconds  int64 `json:"healthy_median_p99_ns"`
	HealthyOverheadNanoseconds   int64 `json:"healthy_overhead_ns"`
}

type runnerMetadata struct {
	RunnerClass  string `json:"runner_class"`
	GoVersion    string `json:"go_version"`
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	LogicalCPUs  int    `json:"logical_cpus"`
	CPUModel     string `json:"cpu_model"`
	RAMBytes     uint64 `json:"ram_bytes"`
	PowerMode    string `json:"power_mode"`
}

func TestAnalyticsLoadProfile(t *testing.T) {
	if !environmentEnabled(fullProfileEnvironment) {
		t.Skipf("full analytics load profile disabled; set %s=1 and install the CPA test adapter", fullProfileEnvironment)
	}
	if analyticsLoadAdapterFactory == nil {
		t.Fatalf("%s enables the full profile, but no CPA analytics adapter is installed; add test/perf/analytics_load_adapter_test.go and assign analyticsLoadAdapterFactory", fullProfileEnvironment)
	}

	runner := readRunnerMetadata()
	results := make([]modeResult, 0, len(fixedAnalyticsLoadProfile.Modes))
	for _, mode := range fixedAnalyticsLoadProfile.Modes {
		result := runLoadMode(t, fixedAnalyticsLoadProfile, mode, runner)
		results = append(results, result)
		encoded, errMarshal := json.Marshal(result)
		if errMarshal != nil {
			t.Fatalf("encode %s result: %v", mode, errMarshal)
		}
		t.Logf("ANALYTICS_LOAD_RESULT %s", encoded)
	}
	assertProfileGates(t, fixedAnalyticsLoadProfile, results)
	recording := recordedRun{Results: results, Runner: runner}
	encoded, errMarshal := json.Marshal(recording)
	if errMarshal != nil {
		t.Fatalf("encode recorded run: %v", errMarshal)
	}
	fmt.Printf("ANALYTICS_LOAD_RUN %s\n", encoded)
}

func TestAnalyticsLoadProfileContract(t *testing.T) {
	validateProfileContract(t, fixedAnalyticsLoadProfile)
	validateDeterministicUpstream(t)
}

func validateProfileContract(t *testing.T, profile loadProfile) {
	t.Helper()
	if profile.Warmup != 30*time.Second {
		t.Fatalf("warmup = %s, want 30s", profile.Warmup)
	}
	if profile.Measurement != 5*time.Minute {
		t.Fatalf("measurement = %s, want 5m", profile.Measurement)
	}
	if profile.Clients != 64 {
		t.Fatalf("clients = %d, want 64", profile.Clients)
	}
	if profile.CompletedPerSecond != 1000 {
		t.Fatalf("completed requests/s = %d, want 1000", profile.CompletedPerSecond)
	}

	measurementRequests := profile.requestCount(profile.Measurement)
	if measurementRequests < 300_000 {
		t.Fatalf("measurement requests = %d, want at least 300000", measurementRequests)
	}
	wantModes := []loadMode{modeDisabled, modeHealthy, modeSQLiteBlocked, modeBothQueuesSaturated}
	if fmt.Sprint(profile.Modes) != fmt.Sprint(wantModes) {
		t.Fatalf("modes = %v, want %v", profile.Modes, wantModes)
	}

	counts := trafficCounts(measurementRequests)
	wantCounts := map[trafficKind]int{
		trafficJSON:         measurementRequests * 60 / 100,
		trafficSSE:          measurementRequests * 25 / 100,
		trafficWebSocket:    measurementRequests * 10 / 100,
		trafficFailureRetry: measurementRequests * 5 / 100,
	}
	for kind, want := range wantCounts {
		if counts[kind] != want {
			t.Errorf("%s requests = %d, want %d", kind, counts[kind], want)
		}
	}
}

func (p loadProfile) requestCount(duration time.Duration) int {
	return int(duration/time.Second) * p.CompletedPerSecond
}

func trafficCounts(total int) map[trafficKind]int {
	counts := make(map[trafficKind]int, 4)
	for sequence := 0; sequence < total; sequence++ {
		counts[trafficForSequence(sequence)]++
	}
	return counts
}

func trafficForSequence(sequence int) trafficKind {
	switch slot := sequence % 100; {
	case slot < 60:
		return trafficJSON
	case slot < 85:
		return trafficSSE
	case slot < 95:
		return trafficWebSocket
	default:
		return trafficFailureRetry
	}
}

func environmentEnabled(name string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	return value == "1" || strings.EqualFold(value, "true")
}

type deterministicUpstream struct {
	server            *httptest.Server
	websocketURL      string
	jsonRequests      atomic.Uint64
	sseRequests       atomic.Uint64
	websocketRequests atomic.Uint64
	failureAttempts   atomic.Uint64
	retryMu           sync.Mutex
	retryAttempts     map[string]int
}

type upstreamCounts struct {
	JSON            uint64 `json:"json"`
	SSE             uint64 `json:"sse"`
	WebSocket       uint64 `json:"websocket"`
	FailureAttempts uint64 `json:"failure_attempts"`
}

func newDeterministicUpstream(t *testing.T) *deterministicUpstream {
	t.Helper()
	upstream := &deterministicUpstream{retryAttempts: make(map[string]int)}
	mux := http.NewServeMux()
	mux.HandleFunc("/json", upstream.handleJSON)
	mux.HandleFunc("/sse", upstream.handleSSE)
	mux.HandleFunc("/ws", upstream.handleWebSocket)
	mux.HandleFunc("/failure", upstream.handleFailure)
	upstream.server = httptest.NewServer(mux)
	upstream.websocketURL = "ws" + strings.TrimPrefix(upstream.server.URL, "http") + "/ws"
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *deterministicUpstream) endpoints() upstreamEndpoints {
	return upstreamEndpoints{
		JSONURL:      u.server.URL + "/json",
		SSEURL:       u.server.URL + "/sse",
		WebSocketURL: u.websocketURL,
		FailureURL:   u.server.URL + "/failure",
	}
}

func (u *deterministicUpstream) handleJSON(writer http.ResponseWriter, _ *http.Request) {
	u.jsonRequests.Add(1)
	writer.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(writer, `{"id":"deterministic","usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}`)
}

func (u *deterministicUpstream) handleSSE(writer http.ResponseWriter, _ *http.Request) {
	u.sseRequests.Add(1)
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(writer, "data: {\"delta\":\"fixture\"}\n\ndata: [DONE]\n\n")
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (u *deterministicUpstream) handleWebSocket(writer http.ResponseWriter, request *http.Request) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	connection, errUpgrade := upgrader.Upgrade(writer, request, nil)
	if errUpgrade != nil {
		return
	}
	defer func() { _ = connection.Close() }()
	u.websocketRequests.Add(1)
	messageType, _, errRead := connection.ReadMessage()
	if errRead != nil {
		return
	}
	_ = connection.WriteMessage(messageType, []byte(`{"delta":"fixture","done":true}`))
}

func (u *deterministicUpstream) handleFailure(writer http.ResponseWriter, request *http.Request) {
	id := strings.TrimSpace(request.Header.Get("X-CPA-Perf-Request-ID"))
	if id == "" {
		http.Error(writer, "missing performance request ID", http.StatusBadRequest)
		return
	}
	u.failureAttempts.Add(1)
	u.retryMu.Lock()
	u.retryAttempts[id]++
	attempt := u.retryAttempts[id]
	u.retryMu.Unlock()
	switch attempt {
	case 1:
		http.Error(writer, "deterministic retry", http.StatusServiceUnavailable)
	case 2:
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"retry-success","usage":{"total_tokens":5}}`)
	default:
		http.Error(writer, "unexpected extra retry", http.StatusConflict)
	}
}

func (u *deterministicUpstream) reset() {
	u.jsonRequests.Store(0)
	u.sseRequests.Store(0)
	u.websocketRequests.Store(0)
	u.failureAttempts.Store(0)
	u.retryMu.Lock()
	u.retryAttempts = make(map[string]int)
	u.retryMu.Unlock()
}

func (u *deterministicUpstream) counts() upstreamCounts {
	return upstreamCounts{
		JSON:            u.jsonRequests.Load(),
		SSE:             u.sseRequests.Load(),
		WebSocket:       u.websocketRequests.Load(),
		FailureAttempts: u.failureAttempts.Load(),
	}
}

func validateDeterministicUpstream(t *testing.T) {
	t.Helper()
	upstream := newDeterministicUpstream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	getAndDrain := func(rawURL string, expectedStatus int, requestID string) {
		request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if errRequest != nil {
			t.Fatalf("create upstream request: %v", errRequest)
		}
		if requestID != "" {
			request.Header.Set("X-CPA-Perf-Request-ID", requestID)
		}
		response, errDo := http.DefaultClient.Do(request)
		if errDo != nil {
			t.Fatalf("call deterministic upstream: %v", errDo)
		}
		_, errRead := io.Copy(io.Discard, response.Body)
		errClose := response.Body.Close()
		if errRead != nil {
			t.Fatalf("read deterministic upstream: %v", errRead)
		}
		if errClose != nil {
			t.Fatalf("close deterministic upstream body: %v", errClose)
		}
		if response.StatusCode != expectedStatus {
			t.Fatalf("deterministic upstream status = %d, want %d", response.StatusCode, expectedStatus)
		}
	}

	getAndDrain(upstream.endpoints().JSONURL, http.StatusOK, "")
	getAndDrain(upstream.endpoints().SSEURL, http.StatusOK, "")
	getAndDrain(upstream.endpoints().FailureURL, http.StatusServiceUnavailable, "contract-retry")
	getAndDrain(upstream.endpoints().FailureURL, http.StatusOK, "contract-retry")

	connection, _, errDial := websocket.DefaultDialer.DialContext(ctx, upstream.endpoints().WebSocketURL, nil)
	if errDial != nil {
		t.Fatalf("dial deterministic WebSocket upstream: %v", errDial)
	}
	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte("fixture")); errWrite != nil {
		_ = connection.Close()
		t.Fatalf("write deterministic WebSocket upstream: %v", errWrite)
	}
	if _, _, errRead := connection.ReadMessage(); errRead != nil {
		_ = connection.Close()
		t.Fatalf("read deterministic WebSocket upstream: %v", errRead)
	}
	if errClose := connection.Close(); errClose != nil {
		t.Fatalf("close deterministic WebSocket upstream: %v", errClose)
	}

	counts := upstream.counts()
	want := upstreamCounts{JSON: 1, SSE: 1, WebSocket: 1, FailureAttempts: 2}
	if counts != want {
		t.Fatalf("deterministic upstream counts = %+v, want %+v", counts, want)
	}
}

type requestOutcome struct {
	latency     time.Duration
	dispatchLag time.Duration
	err         error
}

type phaseResult struct {
	Completed      int
	Elapsed        time.Duration
	P99Latency     time.Duration
	P99DispatchLag time.Duration
}

func runLoadMode(t *testing.T, profile loadProfile, mode loadMode, runner runnerMetadata) modeResult {
	t.Helper()
	upstream := newDeterministicUpstream(t)
	adapter, errFactory := analyticsLoadAdapterFactory(analyticsAdapterConfig{Mode: mode, Upstream: upstream.endpoints()})
	if errFactory != nil {
		t.Fatalf("start %s adapter: %v", mode, errFactory)
	}
	if adapter == nil {
		t.Fatalf("start %s adapter: factory returned nil", mode)
	}

	goroutinesBefore := runtime.NumGoroutine()
	warmupContext, cancelWarmup := context.WithTimeout(context.Background(), profile.Warmup+time.Minute)
	warmup := runTrafficPhase(t, warmupContext, adapter, mode, "warmup", profile.Warmup, profile.requestCount(profile.Warmup), profile, false)
	cancelWarmup()
	if warmup.Completed != profile.requestCount(profile.Warmup) {
		t.Fatalf("%s warmup completed %d requests, want %d", mode, warmup.Completed, profile.requestCount(profile.Warmup))
	}
	upstream.reset()

	metricsBefore := adapter.Metrics()
	sampler := startGoroutineSampler(profile.GoroutineSampleRate)
	measurementContext, cancelMeasurement := context.WithTimeout(context.Background(), profile.Measurement+2*time.Minute)
	measurement := runTrafficPhase(t, measurementContext, adapter, mode, "measurement", profile.Measurement, profile.requestCount(profile.Measurement), profile, true)
	cancelMeasurement()
	goroutineSamples := sampler.stop()
	metricsAfter := adapter.Metrics()

	runtime.GC()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)

	shutdownStart := time.Now()
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), profile.ShutdownTimeout)
	errClose := adapter.Close(shutdownContext)
	cancelShutdown()
	shutdownDuration := time.Since(shutdownStart)
	if errClose != nil {
		t.Fatalf("close %s adapter after %s: %v", mode, shutdownDuration, errClose)
	}
	upstream.server.Close()
	runtime.GC()
	goroutinesAfter := settledGoroutineCount(2 * time.Second)

	queueWaitCount := counterDelta(t, mode, "queue wait", metricsBefore.QueueWaitCount, metricsAfter.QueueWaitCount)
	queueWaitTotal := durationDelta(t, mode, "queue wait total", metricsBefore.QueueWaitTotal, metricsAfter.QueueWaitTotal)
	result := modeResult{
		Mode:                          mode,
		Completed:                     measurement.Completed,
		ActualCompletedPerSecond:      float64(measurement.Completed) / measurement.Elapsed.Seconds(),
		P99LatencyNanoseconds:         measurement.P99Latency.Nanoseconds(),
		P99DispatchLagNanoseconds:     measurement.P99DispatchLag.Nanoseconds(),
		HeapAfterGCBytes:              memory.HeapAlloc,
		GoroutinesBefore:              goroutinesBefore,
		GoroutinesMaximum:             maximumGoroutines(goroutineSamples),
		GoroutinesFinalSlopePerMinute: goroutineSlope(goroutineSamples, 2*time.Minute),
		GoroutinesAfterShutdown:       goroutinesAfter,
		ShutdownNanoseconds:           shutdownDuration.Nanoseconds(),
		QueueWaitCount:                queueWaitCount,
		QueueWaitTotalNanoseconds:     queueWaitTotal.Nanoseconds(),
		QueueWaitMaxNanoseconds:       metricsAfter.QueueWaitMax.Nanoseconds(),
		GenericQueueCapacity:          metricsAfter.GenericQueueCapacity,
		GenericQueueDepth:             metricsAfter.GenericQueueDepth,
		AnalyticsQueueCapacity:        metricsAfter.AnalyticsQueueCapacity,
		AnalyticsQueueDepth:           metricsAfter.AnalyticsQueueDepth,
		GenericQueueDropped:           counterDelta(t, mode, "generic queue dropped", metricsBefore.GenericQueueDropped, metricsAfter.GenericQueueDropped),
		AnalyticsQueueDropped:         counterDelta(t, mode, "analytics queue dropped", metricsBefore.AnalyticsQueueDropped, metricsAfter.AnalyticsQueueDropped),
		GenericLaneWorkers:            metricsAfter.GenericLaneWorkers,
		Upstream:                      upstream.counts(),
		Runner:                        runner,
	}
	validateModeResult(t, profile, result, metricsAfter)
	return result
}

func runTrafficPhase(t *testing.T, ctx context.Context, adapter analyticsLoadAdapter, mode loadMode, phase string, duration time.Duration, total int, profile loadProfile, recordLatency bool) phaseResult {
	t.Helper()
	tasks := make(chan loadRequest, profile.Clients*2)
	outcomes := make(chan requestOutcome, profile.Clients*2)
	var workers sync.WaitGroup
	workers.Add(profile.Clients)
	for worker := 0; worker < profile.Clients; worker++ {
		go func() {
			defer workers.Done()
			for request := range tasks {
				started := time.Now()
				dispatchLag := started.Sub(request.ScheduledAt)
				if dispatchLag < 0 {
					dispatchLag = 0
				}
				response, errDo := adapter.Do(ctx, request)
				latency := time.Since(started)
				if errDo == nil {
					errDo = validateLoadResponse(request, response)
				}
				outcomes <- requestOutcome{latency: latency, dispatchLag: dispatchLag, err: errDo}
			}
		}()
	}

	type collectedOutcomes struct {
		latencies    []time.Duration
		dispatchLags []time.Duration
		completed    int
		errorCount   int
		firstError   error
	}
	collectedChannel := make(chan collectedOutcomes, 1)
	go func() {
		collected := collectedOutcomes{}
		if recordLatency {
			collected.latencies = make([]time.Duration, 0, total)
			collected.dispatchLags = make([]time.Duration, 0, total)
		}
		for outcome := range outcomes {
			if outcome.err != nil {
				collected.errorCount++
				if collected.firstError == nil {
					collected.firstError = outcome.err
				}
				continue
			}
			collected.completed++
			if recordLatency {
				collected.latencies = append(collected.latencies, outcome.latency)
				collected.dispatchLags = append(collected.dispatchLags, outcome.dispatchLag)
			}
		}
		collectedChannel <- collected
	}()

	started := time.Now()
	// Prime one request per client, then pace the remaining completions. This
	// leaves one client-latency window at the end of the measured interval.
	const dispatchBatch = 8
	for first := 0; first < total; first += dispatchBatch {
		batchEnd := min(first+dispatchBatch, total)
		pacedRequests := batchEnd - profile.Clients
		if pacedRequests < 0 {
			pacedRequests = 0
		}
		deadline := started.Add(time.Duration(pacedRequests) * time.Second / time.Duration(profile.CompletedPerSecond))
		if errWait := waitUntil(ctx, deadline); errWait != nil {
			close(tasks)
			workers.Wait()
			close(outcomes)
			<-collectedChannel
			t.Fatalf("%s %s dispatch stopped after %d requests: %v", mode, phase, first, errWait)
		}
		for sequence := first; sequence < batchEnd; sequence++ {
			request := loadRequest{
				ID:          fmt.Sprintf("%s-%s-%09d", mode, phase, sequence),
				Kind:        trafficForSequence(sequence),
				ScheduledAt: deadline,
			}
			select {
			case tasks <- request:
			case <-ctx.Done():
				close(tasks)
				workers.Wait()
				close(outcomes)
				<-collectedChannel
				t.Fatalf("%s %s enqueue stopped after %d requests: %v", mode, phase, sequence, ctx.Err())
			}
		}
	}
	close(tasks)
	workers.Wait()
	close(outcomes)
	collected := <-collectedChannel
	elapsed := time.Since(started)
	if collected.errorCount > 0 {
		t.Fatalf("%s %s had %d client errors; first error: %v", mode, phase, collected.errorCount, collected.firstError)
	}
	return phaseResult{
		Completed:      collected.completed,
		Elapsed:        elapsed,
		P99Latency:     percentile99(collected.latencies),
		P99DispatchLag: percentile99(collected.dispatchLags),
	}
}

func validateLoadResponse(request loadRequest, response loadResponse) error {
	if !response.Success {
		return fmt.Errorf("%s response was not successful", request.Kind)
	}
	if response.Kind != request.Kind {
		return fmt.Errorf("response kind = %s, want %s", response.Kind, request.Kind)
	}
	wantAttempts := 1
	if request.Kind == trafficFailureRetry {
		wantAttempts = 2
	}
	if response.Attempts != wantAttempts {
		return fmt.Errorf("%s attempts = %d, want %d", request.Kind, response.Attempts, wantAttempts)
	}
	return nil
}

func waitUntil(ctx context.Context, deadline time.Time) error {
	wait := time.Until(deadline)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func percentile99(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(left, right int) bool { return values[left] < values[right] })
	index := (99*len(values) + 99) / 100
	return values[index-1]
}

type goroutineSample struct {
	at    time.Time
	count int
}

type goroutineSampler struct {
	stopChannel chan struct{}
	doneChannel chan []goroutineSample
}

func startGoroutineSampler(interval time.Duration) goroutineSampler {
	stopChannel := make(chan struct{})
	doneChannel := make(chan []goroutineSample, 1)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		samples := []goroutineSample{{at: time.Now(), count: runtime.NumGoroutine()}}
		for {
			select {
			case sampledAt := <-ticker.C:
				samples = append(samples, goroutineSample{at: sampledAt, count: runtime.NumGoroutine()})
			case <-stopChannel:
				samples = append(samples, goroutineSample{at: time.Now(), count: runtime.NumGoroutine()})
				doneChannel <- samples
				return
			}
		}
	}()
	return goroutineSampler{stopChannel: stopChannel, doneChannel: doneChannel}
}

func (s goroutineSampler) stop() []goroutineSample {
	close(s.stopChannel)
	return <-s.doneChannel
}

func maximumGoroutines(samples []goroutineSample) int {
	maximum := 0
	for _, sample := range samples {
		if sample.count > maximum {
			maximum = sample.count
		}
	}
	return maximum
}

func goroutineSlope(samples []goroutineSample, window time.Duration) float64 {
	if len(samples) < 2 {
		return 0
	}
	windowStart := samples[len(samples)-1].at.Add(-window)
	first := 0
	for first < len(samples)-1 && samples[first].at.Before(windowStart) {
		first++
	}
	selected := samples[first:]
	if len(selected) < 2 {
		return 0
	}
	origin := selected[0].at
	var sumX, sumY, sumXY, sumXX float64
	for _, sample := range selected {
		x := sample.at.Sub(origin).Minutes()
		y := float64(sample.count)
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	count := float64(len(selected))
	denominator := count*sumXX - sumX*sumX
	if denominator == 0 {
		return 0
	}
	return (count*sumXY - sumX*sumY) / denominator
}

func settledGoroutineCount(maxWait time.Duration) int {
	deadline := time.Now().Add(maxWait)
	previous := runtime.NumGoroutine()
	stable := 0
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		current := runtime.NumGoroutine()
		if current == previous {
			stable++
			if stable == 3 {
				return current
			}
		} else {
			stable = 0
			previous = current
		}
	}
	return runtime.NumGoroutine()
}

func counterDelta(t *testing.T, mode loadMode, name string, before, after uint64) uint64 {
	t.Helper()
	if after < before {
		t.Fatalf("%s %s counter moved backward from %d to %d", mode, name, before, after)
	}
	return after - before
}

func durationDelta(t *testing.T, mode loadMode, name string, before, after time.Duration) time.Duration {
	t.Helper()
	if after < before {
		t.Fatalf("%s %s counter moved backward from %s to %s", mode, name, before, after)
	}
	return after - before
}

func validateModeResult(t *testing.T, profile loadProfile, result modeResult, metrics adapterMetrics) {
	t.Helper()
	wantRequests := profile.requestCount(profile.Measurement)
	if result.Completed < 300_000 || result.Completed != wantRequests {
		t.Errorf("%s completed %d requests, want %d", result.Mode, result.Completed, wantRequests)
	}
	if result.ActualCompletedPerSecond < 1000 {
		t.Errorf("%s completed %.3f requests/s during the measured interval, want at least 1000", result.Mode, result.ActualCompletedPerSecond)
	}
	if result.QueueWaitCount != 0 || result.QueueWaitTotalNanoseconds != 0 || result.QueueWaitMaxNanoseconds != 0 {
		t.Errorf("%s request path waited for analytics queues: count=%d total=%s max=%s", result.Mode, result.QueueWaitCount, time.Duration(result.QueueWaitTotalNanoseconds), time.Duration(result.QueueWaitMaxNanoseconds))
	}
	if result.GoroutinesFinalSlopePerMinute > 0 {
		t.Errorf("%s goroutine slope over the final two minutes is positive: %.4f goroutines/minute", result.Mode, result.GoroutinesFinalSlopePerMinute)
	}
	if result.ShutdownNanoseconds > profile.ShutdownTimeout.Nanoseconds() {
		t.Errorf("%s shutdown took %s, deadline %s", result.Mode, time.Duration(result.ShutdownNanoseconds), profile.ShutdownTimeout)
	}

	wantTraffic := trafficCounts(wantRequests)
	wantUpstream := upstreamCounts{
		JSON:            uint64(wantTraffic[trafficJSON]),
		SSE:             uint64(wantTraffic[trafficSSE]),
		WebSocket:       uint64(wantTraffic[trafficWebSocket]),
		FailureAttempts: uint64(wantTraffic[trafficFailureRetry] * 2),
	}
	if result.Upstream != wantUpstream {
		t.Errorf("%s upstream traffic = %+v, want %+v", result.Mode, result.Upstream, wantUpstream)
	}

	switch result.Mode {
	case modeDisabled:
		if metrics.AnalyticsReady {
			t.Error("disabled mode reports analytics ready")
		}
	case modeHealthy:
		if !metrics.AnalyticsReady {
			t.Error("healthy mode does not report analytics ready")
		}
		if result.GenericQueueCapacity != 4096 || result.AnalyticsQueueCapacity != 8192 {
			t.Errorf("healthy default queue capacities = generic %d, analytics %d; want 4096 and 8192", result.GenericQueueCapacity, result.AnalyticsQueueCapacity)
		}
		if result.GenericQueueDropped != 0 || result.AnalyticsQueueDropped != 0 {
			t.Errorf("healthy mode dropped events: generic=%d analytics=%d", result.GenericQueueDropped, result.AnalyticsQueueDropped)
		}
	case modeSQLiteBlocked:
		if !metrics.SQLiteBlocked {
			t.Error("SQLite-blocked mode does not report the injected block")
		}
		if result.AnalyticsQueueDropped == 0 {
			t.Error("SQLite-blocked mode did not record analytics loss")
		}
	case modeBothQueuesSaturated:
		if result.GenericQueueDropped == 0 || result.AnalyticsQueueDropped == 0 {
			t.Errorf("saturated mode did not drop from both queues: generic=%d analytics=%d", result.GenericQueueDropped, result.AnalyticsQueueDropped)
		}
	}
}

func assertProfileGates(t *testing.T, profile loadProfile, results []modeResult) {
	t.Helper()
	if len(results) != len(profile.Modes) {
		t.Fatalf("profile returned %d modes, want %d", len(results), len(profile.Modes))
	}
	byMode := make(map[loadMode]modeResult, len(results))
	for _, result := range results {
		byMode[result.Mode] = result
	}
	disabled := byMode[modeDisabled]
	if absoluteDifference(disabled.GoroutinesAfterShutdown, disabled.GoroutinesBefore) > 2 {
		t.Errorf("disabled shutdown returned to %d goroutines from %d, want within 2", disabled.GoroutinesAfterShutdown, disabled.GoroutinesBefore)
	}
	for _, mode := range profile.Modes[1:] {
		result := byMode[mode]
		allowedGrowth := result.GenericLaneWorkers + 8
		growth := result.GoroutinesMaximum - disabled.GoroutinesMaximum
		if growth > allowedGrowth {
			t.Errorf("%s added %d goroutines over disabled, allowed %d", mode, growth, allowedGrowth)
		}
		if absoluteDifference(result.GoroutinesAfterShutdown, disabled.GoroutinesAfterShutdown) > 2 {
			t.Errorf("%s left %d goroutines after shutdown, disabled baseline %d", mode, result.GoroutinesAfterShutdown, disabled.GoroutinesAfterShutdown)
		}
	}
	for _, mode := range []loadMode{modeSQLiteBlocked, modeBothQueuesSaturated} {
		for _, problem := range failureModeGateProblems(disabled, byMode[mode]) {
			t.Error(problem)
		}
	}
}

func failureModeGateProblems(disabled, result modeResult) []string {
	var problems []string
	if result.QueueWaitCount != 0 || result.QueueWaitTotalNanoseconds != 0 || result.QueueWaitMaxNanoseconds != 0 {
		problems = append(problems, fmt.Sprintf("%s request path waited for queues", result.Mode))
	}
	const maximumDispatchLagIncrease = time.Millisecond
	if result.P99DispatchLagNanoseconds > disabled.P99DispatchLagNanoseconds+maximumDispatchLagIncrease.Nanoseconds() {
		problems = append(problems, fmt.Sprintf("%s p99 dispatch lag increased by more than %s", result.Mode, maximumDispatchLagIncrease))
	}
	const maximumHeapIncrease = 160 * 1024 * 1024
	if result.HeapAfterGCBytes > disabled.HeapAfterGCBytes+maximumHeapIncrease {
		problems = append(problems, fmt.Sprintf("%s live heap increased by more than %d bytes", result.Mode, maximumHeapIncrease))
	}
	allowedGrowth := result.GenericLaneWorkers + 8
	if result.GoroutinesMaximum-disabled.GoroutinesMaximum > allowedGrowth {
		problems = append(problems, fmt.Sprintf("%s exceeded its goroutine allowance", result.Mode))
	}
	if result.GoroutinesFinalSlopePerMinute > 0 {
		problems = append(problems, fmt.Sprintf("%s goroutine slope is positive", result.Mode))
	}
	if absoluteDifference(result.GoroutinesAfterShutdown, disabled.GoroutinesAfterShutdown) > 2 {
		problems = append(problems, fmt.Sprintf("%s did not return to the disabled goroutine baseline", result.Mode))
	}
	return problems
}

func aggregateRecordedRuns(profile loadProfile, recordings [][]byte) (aggregateResult, error) {
	if len(recordings) != 5 {
		return aggregateResult{}, fmt.Errorf("recorded runs = %d, want exactly 5", len(recordings))
	}
	disabledP99 := make([]int64, 0, len(recordings))
	healthyP99 := make([]int64, 0, len(recordings))
	var expectedRunner runnerMetadata
	var problems []string
	for index, raw := range recordings {
		var recording recordedRun
		if errUnmarshal := json.Unmarshal(raw, &recording); errUnmarshal != nil {
			return aggregateResult{}, fmt.Errorf("decode recorded run %d: %w", index+1, errUnmarshal)
		}
		if index == 0 {
			expectedRunner = recording.Runner
			if expectedRunner.RunnerClass == "" || expectedRunner.RunnerClass == "unrecorded-local" {
				problems = append(problems, "recorded runs do not name a dedicated runner class")
			}
		} else if recording.Runner != expectedRunner {
			problems = append(problems, fmt.Sprintf("recorded run %d used different runner metadata", index+1))
		}
		byMode := make(map[loadMode]modeResult, len(recording.Results))
		for _, result := range recording.Results {
			if _, duplicate := byMode[result.Mode]; duplicate {
				problems = append(problems, fmt.Sprintf("recorded run %d repeats mode %s", index+1, result.Mode))
			}
			byMode[result.Mode] = result
			if result.Completed < 300_000 {
				problems = append(problems, fmt.Sprintf("recorded run %d mode %s completed %d requests", index+1, result.Mode, result.Completed))
			}
			if result.ActualCompletedPerSecond < 1000 {
				problems = append(problems, fmt.Sprintf("recorded run %d mode %s completed %.3f requests/s", index+1, result.Mode, result.ActualCompletedPerSecond))
			}
		}
		for _, mode := range profile.Modes {
			if _, found := byMode[mode]; !found {
				problems = append(problems, fmt.Sprintf("recorded run %d is missing mode %s", index+1, mode))
			}
		}
		disabled, hasDisabled := byMode[modeDisabled]
		healthy, hasHealthy := byMode[modeHealthy]
		if !hasDisabled || !hasHealthy {
			continue
		}
		disabledP99 = append(disabledP99, disabled.P99LatencyNanoseconds)
		healthyP99 = append(healthyP99, healthy.P99LatencyNanoseconds)
		for _, mode := range []loadMode{modeSQLiteBlocked, modeBothQueuesSaturated} {
			result, found := byMode[mode]
			if !found {
				continue
			}
			for _, problem := range failureModeGateProblems(disabled, result) {
				problems = append(problems, fmt.Sprintf("recorded run %d: %s", index+1, problem))
			}
		}
	}
	if len(disabledP99) != 5 || len(healthyP99) != 5 {
		problems = append(problems, "five disabled and healthy p99 values are required")
	}
	result := aggregateResult{}
	if len(disabledP99) == 5 && len(healthyP99) == 5 {
		result.DisabledMedianP99Nanoseconds = medianInt64(disabledP99)
		result.HealthyMedianP99Nanoseconds = medianInt64(healthyP99)
		result.HealthyOverheadNanoseconds = result.HealthyMedianP99Nanoseconds - result.DisabledMedianP99Nanoseconds
		if result.HealthyOverheadNanoseconds >= time.Millisecond.Nanoseconds() {
			problems = append(problems, fmt.Sprintf("healthy median p99 overhead = %s, want less than 1ms", time.Duration(result.HealthyOverheadNanoseconds)))
		}
	}
	if len(problems) > 0 {
		return result, fmt.Errorf("analytics load gate failed: %s", strings.Join(problems, "; "))
	}
	return result, nil
}

func medianInt64(values []int64) int64 {
	ordered := append([]int64(nil), values...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	return ordered[len(ordered)/2]
}

func TestAnalyticsLoadProfileAggregationContract(t *testing.T) {
	recordings := syntheticRecordedRuns(t, nil)
	result, errAggregate := aggregateRecordedRuns(fixedAnalyticsLoadProfile, recordings)
	if errAggregate != nil {
		t.Fatalf("aggregate valid recordings: %v", errAggregate)
	}
	if result.DisabledMedianP99Nanoseconds != time.Millisecond.Nanoseconds() {
		t.Errorf("disabled median p99 = %s, want 1ms", time.Duration(result.DisabledMedianP99Nanoseconds))
	}
	if result.HealthyOverheadNanoseconds != (500 * time.Microsecond).Nanoseconds() {
		t.Errorf("healthy p99 overhead = %s, want 500us", time.Duration(result.HealthyOverheadNanoseconds))
	}

	tests := []struct {
		name      string
		mutate    func([]recordedRun)
		wantError string
	}{
		{
			name: "healthy median overhead",
			mutate: func(runs []recordedRun) {
				for index := range runs {
					runs[index].Results[1].P99LatencyNanoseconds = 2 * time.Millisecond.Nanoseconds()
				}
			},
			wantError: "healthy median p99 overhead",
		},
		{
			name: "measurement rate",
			mutate: func(runs []recordedRun) {
				runs[0].Results[0].ActualCompletedPerSecond = 999.9
			},
			wantError: "completed 999.900 requests/s",
		},
		{
			name: "blocked queue wait",
			mutate: func(runs []recordedRun) {
				runs[0].Results[2].QueueWaitCount = 1
			},
			wantError: "request path waited",
		},
		{
			name: "saturated dispatch lag",
			mutate: func(runs []recordedRun) {
				runs[0].Results[3].P99DispatchLagNanoseconds = 2 * time.Millisecond.Nanoseconds()
			},
			wantError: "dispatch lag",
		},
		{
			name: "blocked heap",
			mutate: func(runs []recordedRun) {
				runs[0].Results[2].HeapAfterGCBytes += 161 * 1024 * 1024
			},
			wantError: "live heap",
		},
		{
			name: "saturated goroutines",
			mutate: func(runs []recordedRun) {
				runs[0].Results[3].GoroutinesMaximum += 10
			},
			wantError: "goroutine allowance",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recordings := syntheticRecordedRuns(t, test.mutate)
			_, errAggregate := aggregateRecordedRuns(fixedAnalyticsLoadProfile, recordings)
			if errAggregate == nil || !strings.Contains(errAggregate.Error(), test.wantError) {
				t.Fatalf("aggregate error = %v, want text %q", errAggregate, test.wantError)
			}
		})
	}
}

func syntheticRecordedRuns(t *testing.T, mutate func([]recordedRun)) [][]byte {
	t.Helper()
	runner := runnerMetadata{RunnerClass: "ci-perf", GoVersion: "go1.26.0", OS: "linux", Architecture: "amd64", LogicalCPUs: 8, CPUModel: "fixture", RAMBytes: 16 << 30, PowerMode: "performance"}
	disabledP99 := []time.Duration{time.Millisecond, 1100 * time.Microsecond, 900 * time.Microsecond, 1200 * time.Microsecond, 800 * time.Microsecond}
	runs := make([]recordedRun, 5)
	for index := range runs {
		disabled := modeResult{Mode: modeDisabled, Completed: 300_000, ActualCompletedPerSecond: 1000, P99LatencyNanoseconds: disabledP99[index].Nanoseconds(), P99DispatchLagNanoseconds: (100 * time.Microsecond).Nanoseconds(), HeapAfterGCBytes: 10 << 20, GoroutinesMaximum: 100, GoroutinesBefore: 10, GoroutinesAfterShutdown: 10, Runner: runner}
		healthy := modeResult{Mode: modeHealthy, Completed: 300_000, ActualCompletedPerSecond: 1000, P99LatencyNanoseconds: (1500 * time.Microsecond).Nanoseconds(), P99DispatchLagNanoseconds: (150 * time.Microsecond).Nanoseconds(), HeapAfterGCBytes: 20 << 20, GoroutinesMaximum: 105, GoroutinesAfterShutdown: 10, GenericLaneWorkers: 1, Runner: runner}
		blocked := modeResult{Mode: modeSQLiteBlocked, Completed: 300_000, ActualCompletedPerSecond: 1000, P99LatencyNanoseconds: (1600 * time.Microsecond).Nanoseconds(), P99DispatchLagNanoseconds: (200 * time.Microsecond).Nanoseconds(), HeapAfterGCBytes: 30 << 20, GoroutinesMaximum: 105, GoroutinesAfterShutdown: 10, GenericLaneWorkers: 1, Runner: runner}
		saturated := modeResult{Mode: modeBothQueuesSaturated, Completed: 300_000, ActualCompletedPerSecond: 1000, P99LatencyNanoseconds: (1700 * time.Microsecond).Nanoseconds(), P99DispatchLagNanoseconds: (200 * time.Microsecond).Nanoseconds(), HeapAfterGCBytes: 40 << 20, GoroutinesMaximum: 105, GoroutinesAfterShutdown: 10, GenericLaneWorkers: 1, Runner: runner}
		runs[index] = recordedRun{Results: []modeResult{disabled, healthy, blocked, saturated}, Runner: runner}
	}
	if mutate != nil {
		mutate(runs)
	}
	recordings := make([][]byte, len(runs))
	for index, run := range runs {
		encoded, errMarshal := json.Marshal(run)
		if errMarshal != nil {
			t.Fatalf("encode synthetic recorded run: %v", errMarshal)
		}
		recordings[index] = encoded
	}
	return recordings
}

func TestAnalyticsLoadProfileRecordedGate(t *testing.T) {
	path := strings.TrimSpace(os.Getenv(recordedResultsEnvironment))
	if path == "" {
		t.Skipf("set %s to a file containing five ANALYTICS_LOAD_RUN JSON records", recordedResultsEnvironment)
	}
	content, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read recorded analytics runs: %v", errRead)
	}
	var recordings [][]byte
	for _, line := range strings.Split(string(content), "\n") {
		marker := strings.Index(line, "ANALYTICS_LOAD_RUN ")
		if marker < 0 {
			continue
		}
		recordings = append(recordings, []byte(strings.TrimSpace(line[marker+len("ANALYTICS_LOAD_RUN "):])))
	}
	result, errAggregate := aggregateRecordedRuns(fixedAnalyticsLoadProfile, recordings)
	if errAggregate != nil {
		t.Fatal(errAggregate)
	}
	encoded, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		t.Fatalf("encode aggregate result: %v", errMarshal)
	}
	t.Logf("ANALYTICS_LOAD_AGGREGATE %s", encoded)
}

func absoluteDifference(left, right int) int {
	if left < right {
		return right - left
	}
	return left - right
}

func readRunnerMetadata() runnerMetadata {
	metadata := runnerMetadata{
		RunnerClass:  strings.TrimSpace(os.Getenv("CPA_PERF_RUNNER_CLASS")),
		GoVersion:    runtime.Version(),
		OS:           runtime.GOOS,
		Architecture: runtime.GOARCH,
		LogicalCPUs:  runtime.NumCPU(),
		CPUModel:     readProcValue("/proc/cpuinfo", "model name"),
		RAMBytes:     readMemoryBytes(),
		PowerMode:    readFirstFile("/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor"),
	}
	if metadata.RunnerClass == "" {
		metadata.RunnerClass = "unrecorded-local"
	}
	if metadata.CPUModel == "" {
		metadata.CPUModel = "unknown"
	}
	if metadata.PowerMode == "" {
		metadata.PowerMode = "unknown"
	}
	return metadata
}

func readProcValue(path, key string) string {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		name, value, found := strings.Cut(scanner.Text(), ":")
		if found && strings.TrimSpace(name) == key {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func readMemoryBytes() uint64 {
	value := readProcValue("/proc/meminfo", "MemTotal")
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return 0
	}
	kibibytes, errParse := strconv.ParseUint(fields[0], 10, 64)
	if errParse != nil {
		return 0
	}
	return kibibytes * 1024
}

func readFirstFile(path string) string {
	content, errRead := os.ReadFile(path)
	if errRead != nil {
		return ""
	}
	return strings.TrimSpace(string(content))
}
