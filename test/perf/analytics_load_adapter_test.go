package perf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const performanceAPIKey = "cpa_perf_93fb8daf0bc34a26acafaa4ff36c96bd94d58e29994f4a55b78fd889f4747440"

type usagePluginFunc func(context.Context, coreusage.Record)

func (fn usagePluginFunc) HandleUsage(ctx context.Context, record coreusage.Record) {
	fn(ctx, record)
}

type blockedSQLiteStore struct {
	*store.SQLiteStore
	release       <-chan struct{}
	blockDuration time.Duration
}

func (s *blockedSQLiteStore) WriteBatch(ctx context.Context, _ []model.Event) error {
	timer := time.NewTimer(s.blockDuration)
	defer timer.Stop()
	select {
	case <-s.release:
		return injectedBlockedStorageError{}
	case <-timer.C:
		return injectedBlockedStorageError{}
	case <-ctx.Done():
		return ctx.Err()
	}
}

type injectedBlockedStorageError struct{}

func (injectedBlockedStorageError) Error() string    { return "injected blocked SQLite writer" }
func (injectedBlockedStorageError) Permanent() bool  { return true }
func (injectedBlockedStorageError) Category() string { return "storage_io" }

type cpaAnalyticsLoadAdapter struct {
	mode               loadMode
	upstream           upstreamEndpoints
	client             *http.Client
	transport          *http.Transport
	websocketDialer    *websocket.Dialer
	usageManager       *coreusage.Manager
	analytics          cpauk.Service
	unregisterTap      coreusage.UnregisterFunc
	sqliteRelease      chan struct{}
	genericRelease     chan struct{}
	releaseSQLiteOnce  sync.Once
	releaseGenericOnce sync.Once
	closed             atomic.Bool
}

func init() {
	analyticsLoadAdapterFactory = newCPAAnalyticsLoadAdapter
}

func newCPAAnalyticsLoadAdapter(config analyticsAdapterConfig) (analyticsLoadAdapter, error) {
	if config.StateDirectory == "" {
		return nil, fmt.Errorf("performance adapter state directory is required")
	}
	genericCapacity := config.GenericQueueCapacity
	if genericCapacity <= 0 {
		genericCapacity = coreusage.DefaultObserverQueueCapacity
	}
	analyticsCapacity := config.AnalyticsQueueCapacity
	if analyticsCapacity <= 0 {
		analyticsCapacity = cpauk.DefaultQueueCapacity
	}
	blockDuration := config.SQLiteBlockDuration
	if blockDuration <= 0 {
		blockDuration = 10 * time.Second
	}

	moduleConfig := cpauk.DefaultConfig()
	moduleConfig.Enabled = config.Mode != modeDisabled
	moduleConfig.Path = filepath.Join(config.StateDirectory, "analytics.db")
	moduleConfig.QueueCapacity = analyticsCapacity
	if moduleConfig.BatchSize > analyticsCapacity {
		moduleConfig.BatchSize = analyticsCapacity
	}
	moduleConfig.MaxStorageBytes = 512 << 20
	moduleConfig.MinFreeBytes = 64 << 20

	sqliteRelease := make(chan struct{})
	factory := func(ctx context.Context, moduleConfig cpauk.Config) (cpauk.Backend, [32]byte, error) {
		database, errOpen := store.Open(ctx, store.Config{
			Path:            moduleConfig.Path,
			IdentityKeyPath: filepath.Join(filepath.Dir(moduleConfig.Path), "identity.key"),
			MaxStorageBytes: moduleConfig.MaxStorageBytes,
			MinFreeBytes:    moduleConfig.MinFreeBytes,
		})
		if errOpen != nil {
			return nil, [32]byte{}, errOpen
		}
		backend := cpauk.Backend(database)
		if config.Mode == modeSQLiteBlocked || config.Mode == modeBothQueuesSaturated {
			backend = &blockedSQLiteStore{SQLiteStore: database, release: sqliteRelease, blockDuration: blockDuration}
		}
		return backend, database.IdentityKeyArray(), nil
	}

	analytics := cpauk.New(context.Background(), moduleConfig, factory)
	if errReady := waitForAnalyticsState(analytics, moduleConfig.Enabled, 10*time.Second); errReady != nil {
		close(sqliteRelease)
		closeContext, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
		_ = analytics.Close(closeContext)
		cancelClose()
		return nil, errReady
	}

	manager := coreusage.NewManager(genericCapacity)
	genericRelease := make(chan struct{})
	genericObserver := usagePluginFunc(func(context.Context, coreusage.Record) {})
	if config.Mode == modeBothQueuesSaturated {
		genericObserver = usagePluginFunc(func(context.Context, coreusage.Record) {
			<-genericRelease
		})
	}
	if _, errRegister := manager.RegisterNamed("performance-generic", genericObserver); errRegister != nil {
		close(sqliteRelease)
		close(genericRelease)
		closeContext, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
		_ = analytics.Close(closeContext)
		cancelClose()
		return nil, fmt.Errorf("register generic usage observer: %w", errRegister)
	}
	unregisterTap, errRegister := manager.RegisterSanitizerTapNamed("performance-cpauk", analytics.Observer())
	if errRegister != nil {
		close(sqliteRelease)
		close(genericRelease)
		closeContext, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
		_ = manager.Close(closeContext)
		_ = analytics.Close(closeContext)
		cancelClose()
		return nil, fmt.Errorf("register analytics sanitizer tap: %w", errRegister)
	}
	manager.Start(context.Background())

	transport := &http.Transport{
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 128,
		ForceAttemptHTTP2:   false,
		DisableCompression:  true,
	}
	return &cpaAnalyticsLoadAdapter{
		mode:            config.Mode,
		upstream:        config.Upstream,
		client:          &http.Client{Transport: transport},
		transport:       transport,
		websocketDialer: &websocket.Dialer{HandshakeTimeout: 5 * time.Second},
		usageManager:    manager,
		analytics:       analytics,
		unregisterTap:   unregisterTap,
		sqliteRelease:   sqliteRelease,
		genericRelease:  genericRelease,
	}, nil
}

func waitForAnalyticsState(service cpauk.Service, enabled bool, timeout time.Duration) error {
	if !enabled {
		if service.Capabilities().State != model.StateDisabled {
			return fmt.Errorf("disabled analytics state = %s", service.Capabilities().State)
		}
		return nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		capabilities := service.Capabilities()
		switch capabilities.State {
		case model.StateReady:
			return nil
		case model.StateCircuitOpen:
			return fmt.Errorf("analytics startup entered circuit_open: category=%s", service.Health().Category)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("analytics startup did not reach ready within %s", timeout)
}

func (a *cpaAnalyticsLoadAdapter) Do(ctx context.Context, request loadRequest) (loadResponse, error) {
	if a == nil || a.closed.Load() {
		return loadResponse{}, errors.New("performance adapter is closed")
	}
	switch request.Kind {
	case trafficJSON:
		return a.doHTTP(ctx, request, a.upstream.JSONURL, http.StatusOK, 1)
	case trafficSSE:
		return a.doHTTP(ctx, request, a.upstream.SSEURL, http.StatusOK, 1)
	case trafficFailureRetry:
		first, errFirst := a.doHTTP(ctx, request, a.upstream.FailureURL, http.StatusServiceUnavailable, 1)
		if errFirst != nil {
			return first, errFirst
		}
		second, errSecond := a.doHTTP(ctx, request, a.upstream.FailureURL, http.StatusOK, 1)
		second.Attempts = 2
		return second, errSecond
	case trafficWebSocket:
		return a.doWebSocket(ctx, request)
	default:
		return loadResponse{}, fmt.Errorf("unsupported traffic kind %q", request.Kind)
	}
}

func (a *cpaAnalyticsLoadAdapter) doHTTP(ctx context.Context, load loadRequest, rawURL string, expectedStatus, attempts int) (loadResponse, error) {
	started := time.Now()
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if errRequest != nil {
		return loadResponse{}, fmt.Errorf("create %s upstream request: %w", load.Kind, errRequest)
	}
	request.Header.Set("X-CPA-Perf-Request-ID", load.ID)
	response, errDo := a.client.Do(request)
	latency := time.Since(started)
	if errDo != nil {
		a.publishAttempt(load, started, latency, 0, true)
		return loadResponse{Kind: load.Kind, Attempts: attempts}, fmt.Errorf("call %s upstream: %w", load.Kind, errDo)
	}
	_, errRead := io.Copy(io.Discard, response.Body)
	errClose := response.Body.Close()
	failed := response.StatusCode >= http.StatusBadRequest
	a.publishAttempt(load, started, latency, response.StatusCode, failed)
	if errRead != nil {
		return loadResponse{Kind: load.Kind, Attempts: attempts}, fmt.Errorf("read %s upstream: %w", load.Kind, errRead)
	}
	if errClose != nil {
		return loadResponse{Kind: load.Kind, Attempts: attempts}, fmt.Errorf("close %s upstream response: %w", load.Kind, errClose)
	}
	if response.StatusCode != expectedStatus {
		return loadResponse{Kind: load.Kind, Attempts: attempts}, fmt.Errorf("%s upstream status = %d, want %d", load.Kind, response.StatusCode, expectedStatus)
	}
	return loadResponse{Kind: load.Kind, Attempts: attempts, Success: true}, nil
}

func (a *cpaAnalyticsLoadAdapter) doWebSocket(ctx context.Context, load loadRequest) (loadResponse, error) {
	started := time.Now()
	connection, response, errDial := a.websocketDialer.DialContext(ctx, a.upstream.WebSocketURL, nil)
	if errDial != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		a.publishAttempt(load, started, time.Since(started), status, true)
		return loadResponse{Kind: load.Kind, Attempts: 1}, fmt.Errorf("dial WebSocket upstream: %w", errDial)
	}
	if errWrite := connection.WriteMessage(websocket.TextMessage, []byte(load.ID)); errWrite != nil {
		_ = connection.Close()
		a.publishAttempt(load, started, time.Since(started), 0, true)
		return loadResponse{Kind: load.Kind, Attempts: 1}, fmt.Errorf("write WebSocket upstream: %w", errWrite)
	}
	if _, _, errRead := connection.ReadMessage(); errRead != nil {
		_ = connection.Close()
		a.publishAttempt(load, started, time.Since(started), 0, true)
		return loadResponse{Kind: load.Kind, Attempts: 1}, fmt.Errorf("read WebSocket upstream: %w", errRead)
	}
	errClose := connection.Close()
	latency := time.Since(started)
	a.publishAttempt(load, started, latency, http.StatusSwitchingProtocols, false)
	if errClose != nil {
		return loadResponse{Kind: load.Kind, Attempts: 1}, fmt.Errorf("close WebSocket upstream: %w", errClose)
	}
	return loadResponse{Kind: load.Kind, Attempts: 1, Success: true}, nil
}

func (a *cpaAnalyticsLoadAdapter) publishAttempt(load loadRequest, requestedAt time.Time, latency time.Duration, status int, failed bool) {
	requestHash := sha256.Sum256([]byte(load.ID))
	requestID := hex.EncodeToString(requestHash[:16])
	detail := coreusage.Detail{TokenQuality: coreusage.TokenQualityMissing}
	if !failed {
		detail = coreusage.Detail{
			InputTokens:  11,
			OutputTokens: 7,
			TotalTokens:  18,
			TokenQuality: coreusage.TokenQualityExact,
		}
	}
	a.usageManager.Publish(ctxWithoutCancellation{}, coreusage.Record{
		ProxyRequestID:   requestID,
		RequestIDQuality: coreusage.RequestIDObserved,
		Provider:         "performance",
		ExecutorType:     "deterministic-upstream",
		Model:            "fixture-model",
		Alias:            "fixture-alias",
		APIKey:           performanceAPIKey,
		AuthIndex:        "performance-credential-0",
		AuthType:         "test",
		EndpointClass:    "responses",
		Generate:         coreusage.GenerateFlag(true),
		RequestedAt:      requestedAt.UTC(),
		Latency:          latency,
		Failed:           failed,
		Fail:             coreusage.Failure{StatusCode: status},
		Detail:           detail,
	})
}

// ctxWithoutCancellation avoids retaining a measurement deadline in generic
// observer snapshots. The load adapter publishes only fixed, non-secret data.
type ctxWithoutCancellation struct{}

func (ctxWithoutCancellation) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctxWithoutCancellation) Done() <-chan struct{}       { return nil }
func (ctxWithoutCancellation) Err() error                  { return nil }
func (ctxWithoutCancellation) Value(any) any               { return nil }

func (a *cpaAnalyticsLoadAdapter) Metrics() adapterMetrics {
	managerStats := a.usageManager.Stats()
	capabilities := a.analytics.Capabilities()
	return adapterMetrics{
		AnalyticsReady:         capabilities.State == model.StateReady,
		AnalyticsState:         string(capabilities.State),
		SQLiteBlocked:          a.mode == modeSQLiteBlocked || a.mode == modeBothQueuesSaturated,
		GenericLaneWorkers:     len(managerStats.Lanes),
		GenericQueueCapacity:   managerStats.QueueCapacity,
		GenericQueueDepth:      managerStats.QueueDepth,
		AnalyticsQueueCapacity: int(capabilities.Queue.Capacity),
		AnalyticsQueueDepth:    int(capabilities.Queue.Depth),
		GenericQueueDropped:    managerStats.Dropped,
		AnalyticsQueueDropped:  uint64(max(capabilities.Queue.Dropped, 0)),
	}
}

func (a *cpaAnalyticsLoadAdapter) Close(ctx context.Context) error {
	if a == nil || !a.closed.CompareAndSwap(false, true) {
		return nil
	}
	a.releaseGenericOnce.Do(func() { close(a.genericRelease) })
	a.releaseSQLiteOnce.Do(func() { close(a.sqliteRelease) })
	if ctx == nil {
		ctx = context.Background()
	}
	var closeErrors []error
	if a.unregisterTap != nil {
		if errUnregister := a.unregisterTap(ctx); errUnregister != nil {
			closeErrors = append(closeErrors, fmt.Errorf("unregister analytics tap: %w", errUnregister))
		}
	}
	if errManager := a.usageManager.Close(ctx); errManager != nil {
		closeErrors = append(closeErrors, fmt.Errorf("close usage manager: %w", errManager))
	}
	if errAnalytics := a.analytics.Close(ctx); errAnalytics != nil {
		closeErrors = append(closeErrors, fmt.Errorf("close analytics service: %w", errAnalytics))
	}
	a.transport.CloseIdleConnections()
	return errors.Join(closeErrors...)
}

func TestCPAAnalyticsLoadAdapterModes(t *testing.T) {
	modes := []loadMode{modeDisabled, modeHealthy, modeSQLiteBlocked, modeBothQueuesSaturated}
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			upstream := newDeterministicUpstream(t)
			adapter, errFactory := newCPAAnalyticsLoadAdapter(analyticsAdapterConfig{
				Mode:                   mode,
				Upstream:               upstream.endpoints(),
				StateDirectory:         t.TempDir(),
				GenericQueueCapacity:   8,
				AnalyticsQueueCapacity: 8,
				SQLiteBlockDuration:    20 * time.Millisecond,
			})
			if errFactory != nil {
				t.Fatalf("start adapter: %v", errFactory)
			}

			requestCount := 4
			if mode == modeSQLiteBlocked || mode == modeBothQueuesSaturated {
				requestCount = 40
			}
			traffic := []trafficKind{trafficJSON, trafficSSE, trafficWebSocket, trafficFailureRetry}
			for sequence := 0; sequence < requestCount; sequence++ {
				kind := traffic[sequence%len(traffic)]
				request := loadRequest{ID: fmt.Sprintf("smoke-%s-%d", mode, sequence), Kind: kind, ScheduledAt: time.Now()}
				response, errDo := adapter.Do(context.Background(), request)
				if errDo != nil {
					t.Fatalf("request %d: %v", sequence, errDo)
				}
				if errValidate := validateLoadResponse(request, response); errValidate != nil {
					t.Fatalf("request %d response: %v", sequence, errValidate)
				}
			}

			if mode == modeSQLiteBlocked || mode == modeBothQueuesSaturated {
				deadline := time.Now().Add(2 * time.Second)
				for (adapter.Metrics().AnalyticsQueueDropped == 0 || adapter.Metrics().AnalyticsState != "circuit_open") && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				if adapter.Metrics().AnalyticsQueueDropped == 0 {
					t.Error("blocked analytics queue did not drop an event")
				}
				if adapter.Metrics().AnalyticsState != "circuit_open" {
					t.Errorf("blocked analytics state = %q, want circuit_open", adapter.Metrics().AnalyticsState)
				}
			}
			if mode == modeBothQueuesSaturated && adapter.Metrics().GenericQueueDropped == 0 {
				t.Error("saturated generic queue did not drop an event")
			}
			metrics := adapter.Metrics()
			if metrics.GenericQueueCapacity != 8 || metrics.AnalyticsQueueCapacity != 8 {
				t.Errorf("queue capacities = generic %d analytics %d, want 8 and 8", metrics.GenericQueueCapacity, metrics.AnalyticsQueueCapacity)
			}
			if mode == modeDisabled && metrics.AnalyticsReady {
				t.Error("disabled adapter reports analytics ready")
			}
			if mode == modeHealthy && !metrics.AnalyticsReady {
				t.Error("healthy adapter does not report analytics ready")
			}

			closeContext, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelClose()
			if errClose := adapter.Close(closeContext); errClose != nil {
				t.Fatalf("close adapter: %v", errClose)
			}
		})
	}
}

func TestRunnerMetadataOverrides(t *testing.T) {
	t.Setenv("CPA_PERF_RUNNER_CLASS", "dedicated-fixture")
	t.Setenv("CPA_PERF_OS", "fixture-os")
	t.Setenv("CPA_PERF_CPU_MODEL", "fixture-cpu")
	t.Setenv("CPA_PERF_RAM_BYTES", "17179869184")
	t.Setenv("CPA_PERF_POWER_MODE", "performance")
	metadata := readRunnerMetadata()
	if metadata.RunnerClass != "dedicated-fixture" || metadata.OS != "fixture-os" || metadata.CPUModel != "fixture-cpu" ||
		metadata.RAMBytes != 17_179_869_184 || metadata.PowerMode != "performance" {
		t.Fatalf("runner metadata overrides = %+v", metadata)
	}
}
