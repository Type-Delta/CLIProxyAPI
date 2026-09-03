package cpauk

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestDisabledServiceIsNonNilAndTyped(t *testing.T) {
	service := NewDisabled()
	if service == nil || service.Observer() == nil || service.Reader() == nil || service.Maintenance() == nil {
		t.Fatal("disabled service returned a nil interface")
	}
	if service.Health().State != StateDisabled || service.Capabilities().Available {
		t.Fatalf("disabled snapshots = %#v %#v", service.Health(), service.Capabilities())
	}
	if _, err := service.Reader().Summary(context.Background(), model.Query{}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled read error = %v", err)
	}
	if _, err := service.Maintenance().Status(context.Background(), "job"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled maintenance error = %v", err)
	}
}

func TestCredentialIdentityHonorsPrivacySetting(t *testing.T) {
	service := &service{identityKey: [32]byte{1}, hasIdentityKey: true}
	credentialID, err := service.CredentialID("provider", "index", "auth-id")
	if err != nil {
		t.Fatal(err)
	}
	if credentialID != nil {
		t.Fatalf("credential identity stored with privacy disabled: %q", *credentialID)
	}
	credentials, err := service.ProviderCredentials(context.Background())
	if err != nil || len(credentials) != 0 {
		t.Fatalf("credential rows with privacy disabled = %v, %v", credentials, err)
	}
	snapshots, err := service.ProviderQuotaSnapshots(context.Background())
	if err != nil || len(snapshots) != 0 {
		t.Fatalf("quota snapshots with privacy disabled = %v, %v", snapshots, err)
	}
	service.config.Privacy.StoreCredentialID = true
	credentialID, err = service.CredentialID("provider", "index", "auth-id")
	if err != nil || credentialID == nil || !model.IsFullKeyID(*credentialID) {
		t.Fatalf("credential identity with privacy enabled = %v, %v", credentialID, err)
	}
}

func TestInvalidConfigAndFactoryFailureStayUnavailable(t *testing.T) {
	badDrain := DefaultConfig()
	badDrain.ShutdownDrain = -time.Second
	if err := New(context.Background(), badDrain, nil).Close(context.Background()); err != nil {
		t.Fatalf("invalid service close escaped analytics failure: %v", err)
	}

	invalid := DefaultConfig()
	invalid.Enabled = true
	invalid.QueueCapacity = -1
	service := New(context.Background(), invalid, func(context.Context, Config) (Backend, [32]byte, error) {
		panic("must not run")
	})
	if got := service.Health(); got.State != StateCircuitOpen || got.Category != "invalid_config" {
		t.Fatalf("invalid config health = %#v", got)
	}
	if _, err := service.Reader().Events(context.Background(), model.Query{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("invalid config read error = %v", err)
	}
	valid := smallConfig()
	if result := service.Reconfigure(valid); !result.Applied || result.Error != nil {
		t.Fatalf("valid replacement after invalid startup = %#v", result)
	}
	// The original factory deliberately panics, so the replacement reaches a
	// redacted unavailable state instead of affecting CPA.
	waitServiceState(t, service, StateCircuitOpen)

	config := smallConfig()
	service = New(context.Background(), config, func(context.Context, Config) (Backend, [32]byte, error) {
		return nil, [32]byte{}, errors.New("path /private/value")
	})
	waitServiceState(t, service, StateCircuitOpen)
	if got := service.Health(); got.Category != "storage_start" || got.Message != "Analytics is unavailable." {
		t.Fatalf("factory failure leaked details or wrong category: %#v", got)
	}

	service = New(context.Background(), config, func(context.Context, Config) (Backend, [32]byte, error) {
		panic("private panic detail")
	})
	waitServiceState(t, service, StateCircuitOpen)
	if got := service.Health().Message; got != "Analytics is unavailable." {
		t.Fatalf("factory panic message = %q", got)
	}

	service = New(context.Background(), config, func(context.Context, Config) (Backend, [32]byte, error) {
		return nil, [32]byte{}, classifiedServiceError{}
	})
	waitServiceState(t, service, StateCircuitOpen)
	if got := service.Health().Category; got != "identity_key" {
		t.Fatalf("classified startup category = %q", got)
	}
}

func TestRestartRequiredMarkerIsVisibleInHealth(t *testing.T) {
	serviceFacade := NewUnavailable("startup", smallConfig())
	serviceFacade.(interface{ MarkRestartRequired([]string) }).MarkRestartRequired([]string{"viewer.trusted-proxy-cidrs"})
	if health := serviceFacade.Health(); health.Category != "restart_required" || health.Message == "" {
		t.Fatalf("restart-required health=%+v", health)
	}
}

func TestIsolatedInvalidServiceStartsAfterCorrectedReload(t *testing.T) {
	config := smallConfig()
	service := NewInvalid("unknown_field", "surprise", config, func(context.Context, Config) (Backend, [32]byte, error) {
		return &fakeBackend{}, [32]byte{1}, nil
	})
	if health := service.Health(); health.Category != "unknown_field" || health.Field != "surprise" {
		t.Fatalf("invalid health = %#v", health)
	}
	result := service.Reconfigure(config)
	if !result.Applied || result.Error != nil {
		t.Fatalf("corrected reload = %#v", result)
	}
	waitServiceState(t, service, StateReady)
	if health := service.Health(); health.Field != "" || health.Category != "" {
		t.Fatalf("recovered health = %#v", health)
	}
}

type classifiedServiceError struct{}

func (classifiedServiceError) Error() string    { return "private path detail" }
func (classifiedServiceError) Permanent() bool  { return true }
func (classifiedServiceError) Category() string { return "identity_key" }

func TestServiceStartsPublishesReadsAndCloses(t *testing.T) {
	backend := &fakeBackend{}
	service := New(context.Background(), smallConfig(), func(context.Context, Config) (Backend, [32]byte, error) {
		return backend, [32]byte{1, 2, 3}, nil
	})
	waitServiceState(t, service, StateReady)

	record := validServiceRecord()
	service.Observer().HandleUsage(context.Background(), record)
	waitForService(t, func() bool { return backend.writes.Load() == 1 && service.Health().LastSuccessfulWrite != nil })
	if health := service.Health(); health.LastSuccessfulWrite == nil || health.Queue.Dropped != 0 {
		t.Fatalf("health after write = %#v", health)
	}
	summary, err := service.Reader().Summary(context.Background(), model.Query{})
	if err != nil || summary.UpstreamAttempts != 7 {
		t.Fatalf("Summary = %#v, %v", summary, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if backend.closes.Load() != 1 {
		t.Fatalf("backend close calls = %d", backend.closes.Load())
	}
	service.Observer().HandleUsage(context.Background(), record)
	if backend.writes.Load() != 1 {
		t.Fatal("observer wrote after close")
	}
}

func TestStartupSerializesKeyLifecyclePublication(t *testing.T) {
	backend := &blockingLifecycleBackend{
		fakeBackend: &fakeBackend{}, firstCall: make(chan struct{}), releaseFirst: make(chan struct{}),
	}
	serviceFacade := New(context.Background(), smallConfig(), func(context.Context, Config) (Backend, [32]byte, error) {
		return backend, [32]byte{1}, nil
	})
	select {
	case <-backend.firstCall:
	case <-time.After(time.Second):
		t.Fatal("startup did not begin lifecycle publication")
	}
	latest := []string{strings.Repeat("a", 64)}
	updated := make(chan error, 1)
	go func() {
		updated <- serviceFacade.(interface {
			UpdateKeyLifecycle(context.Context, []string, []string) error
		}).UpdateKeyLifecycle(context.Background(), latest, nil)
	}()
	close(backend.releaseFirst)
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	waitServiceState(t, serviceFacade, StateReady)
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.calls) != 2 || len(backend.calls[1]) != 1 || backend.calls[1][0] != latest[0] {
		t.Fatalf("lifecycle calls = %#v", backend.calls)
	}
}

func TestWriteCircuitKeepsReadableBackendAvailable(t *testing.T) {
	backend := &fakeBackend{}
	serviceFacade := New(context.Background(), smallConfig(), func(context.Context, Config) (Backend, [32]byte, error) {
		return backend, [32]byte{}, nil
	})
	waitServiceState(t, serviceFacade, StateReady)
	concrete := serviceFacade.(*service)
	concrete.snapshots.mutate(func(health *model.Health) {
		health.State = StateCircuitOpen
		health.Category = "storage_quota"
		health.Queue.Dropped = 3
	})
	if capabilities := serviceFacade.Capabilities(); !capabilities.Available || !capabilities.Degraded {
		t.Fatalf("circuit-open capabilities = %#v", capabilities)
	}

	summary, err := serviceFacade.Reader().Summary(context.Background(), model.Query{})
	if err != nil {
		t.Fatalf("stale read failed: %v", err)
	}
	if !summary.Meta.Degraded {
		t.Fatalf("stale read metadata = %#v", summary.Meta)
	}
}

func TestScheduledRetentionUsesMaintenanceReadGate(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, store.Config{
		Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &blockingRetentionBackend{
		SQLiteStore: database, entered: make(chan struct{}), release: make(chan struct{}),
	}
	serviceFacade := New(ctx, smallConfig(), func(context.Context, Config) (Backend, [32]byte, error) {
		return backend, database.IdentityKeyArray(), nil
	})
	select {
	case <-backend.entered:
	case <-time.After(time.Second):
		t.Fatal("scheduled retention did not start")
	}
	if capabilities := serviceFacade.Capabilities(); capabilities.Available {
		t.Fatalf("maintenance capabilities=%+v", capabilities)
	}
	if _, err = serviceFacade.Reader().Summary(ctx, model.Query{}); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("maintenance read error=%v", err)
	}
	close(backend.release)
	waitServiceState(t, serviceFacade, StateReady)
	closeContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err = serviceFacade.Close(closeContext); err != nil {
		t.Fatal(err)
	}
}

func TestServiceContainsQueryAndClosePanics(t *testing.T) {
	backend := &fakeBackend{panicQuery: true, panicClose: true}
	service := New(context.Background(), smallConfig(), func(context.Context, Config) (Backend, [32]byte, error) {
		return backend, [32]byte{}, nil
	})
	waitServiceState(t, service, StateReady)
	if _, err := service.Reader().Leaderboard(context.Background(), model.Query{}); !errors.Is(err, ErrInternal) {
		t.Fatalf("query panic error = %v", err)
	}
	if health := service.Health(); health.State != StateDegraded || health.Category != "query_panic" {
		t.Fatalf("query panic health = %#v", health)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := service.Close(ctx); !errors.Is(err, ErrInternal) {
		t.Fatalf("close panic error = %v", err)
	}
}

func TestServiceReconfigureIsGenerationAware(t *testing.T) {
	backend := &fakeBackend{}
	closed := make(chan struct{})
	backend.closed = closed
	config := smallConfig()
	service := New(context.Background(), config, func(context.Context, Config) (Backend, [32]byte, error) {
		return backend, [32]byte{}, nil
	})
	waitServiceState(t, service, StateReady)
	oldObserver := service.Observer()

	restart := config
	restart.Path = "different.db"
	result := service.Reconfigure(restart)
	if result.Applied || len(result.RestartRequired) != 1 || result.RestartRequired[0] != "path" {
		t.Fatalf("restart-required result = %#v", result)
	}

	hot := config
	hot.BatchSize = 2
	if result = service.Reconfigure(hot); !result.Applied || result.Error != nil {
		t.Fatalf("hot reconfigure = %#v", result)
	}

	disabled := hot
	disabled.Enabled = false
	if result = service.Reconfigure(disabled); !result.Applied {
		t.Fatalf("disable result = %#v", result)
	}
	oldObserver.HandleUsage(context.Background(), validServiceRecord())
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("old generation did not close")
	}
	if backend.writes.Load() != 0 {
		t.Fatal("late callback crossed the disabled generation")
	}
	if _, err := service.Reader().Summary(context.Background(), model.Query{}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("read after disable = %v", err)
	}
}

func TestServiceCloseReturnsAtDeadlineWhenBackendStalls(t *testing.T) {
	backend := &fakeBackend{stallClose: true}
	config := smallConfig()
	config.ShutdownDrain = 20 * time.Millisecond
	service := New(context.Background(), config, func(context.Context, Config) (Backend, [32]byte, error) {
		return backend, [32]byte{}, nil
	})
	waitServiceState(t, service, StateReady)
	start := time.Now()
	err := service.Close(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Close took %s", elapsed)
	}
}

func smallConfig() Config {
	config := DefaultConfig()
	config.Enabled = true
	config.QueueCapacity = 8
	config.BatchSize = 1
	config.FlushInterval = time.Millisecond
	config.ShutdownDrain = 50 * time.Millisecond
	return config
}

func validServiceRecord() coreusage.Record {
	return coreusage.Record{
		ProxyRequestID:   "d1371f43e6b8362d05d7567ed5fcc2ad",
		EndpointClass:    "responses",
		Provider:         "provider",
		ExecutorType:     "executor",
		Model:            "model",
		APIKey:           "fixture-secret-f10d6a89",
		RequestedAt:      time.Now().UTC(),
		Latency:          time.Millisecond,
		Detail:           coreusage.Detail{TotalTokens: 1},
		RequestIDQuality: coreusage.RequestIDObserved,
	}
}

type fakeBackend struct {
	writes     atomic.Int64
	closes     atomic.Int64
	panicQuery bool
	panicClose bool
	stallClose bool
	closed     chan struct{}
	closeOnce  sync.Once
}

type blockingLifecycleBackend struct {
	*fakeBackend
	mu           sync.Mutex
	calls        [][]string
	firstCall    chan struct{}
	releaseFirst chan struct{}
}

type blockingRetentionBackend struct {
	*store.SQLiteStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingRetentionBackend) ApplyRetentionPolicy(ctx context.Context, raw, hourly time.Time, batchSize int) (store.RetentionResult, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return b.SQLiteStore.ApplyRetentionPolicy(ctx, raw, hourly, batchSize)
	case <-ctx.Done():
		return store.RetentionResult{}, ctx.Err()
	}
}

func (b *blockingLifecycleBackend) UpdateKeyLifecycle(_ context.Context, configured, _ []string) error {
	b.mu.Lock()
	call := len(b.calls)
	b.calls = append(b.calls, append([]string(nil), configured...))
	b.mu.Unlock()
	if call == 0 {
		close(b.firstCall)
		<-b.releaseFirst
	}
	return nil
}

func (b *fakeBackend) WriteBatch(context.Context, []model.Event) error {
	b.writes.Add(1)
	return nil
}

func (b *fakeBackend) Close(ctx context.Context) error {
	b.closes.Add(1)
	if b.panicClose {
		panic("injected close panic")
	}
	if b.stallClose {
		<-ctx.Done()
		return ctx.Err()
	}
	if b.closed != nil {
		b.closeOnce.Do(func() { close(b.closed) })
	}
	return nil
}

func (b *fakeBackend) Summary(context.Context, model.Query) (model.Summary, error) {
	return model.Summary{UpstreamAttempts: 7}, nil
}
func (b *fakeBackend) Timeseries(context.Context, model.Query) (model.Timeseries, error) {
	return model.Timeseries{}, nil
}
func (b *fakeBackend) Dimensions(context.Context, model.Query) (model.DimensionPage, error) {
	return model.DimensionPage{}, nil
}
func (b *fakeBackend) Events(context.Context, model.Query) (model.EventPage, error) {
	return model.EventPage{}, nil
}
func (b *fakeBackend) Leaderboard(context.Context, model.Query) (model.LeaderboardPage, error) {
	if b.panicQuery {
		panic("injected query panic")
	}
	return model.LeaderboardPage{}, nil
}

func waitServiceState(t *testing.T, service Service, state State) {
	t.Helper()
	waitForService(t, func() bool { return service.Health().State == state })
}

func waitForService(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStorageZoneMismatchSurfacesBothZonesInHealth(t *testing.T) {
	config := smallConfig()
	config.Path = filepath.Join(t.TempDir(), "analytics.db")
	serviceFacade := New(context.Background(), config, func(context.Context, Config) (Backend, [32]byte, error) {
		return nil, [32]byte{}, store.ZoneMismatchError{Stored: "UTC", Configured: "Asia/Kolkata"}
	})
	defer func() { _ = serviceFacade.Close(context.Background()) }()
	waitServiceState(t, serviceFacade, StateCircuitOpen)
	health := serviceFacade.Health()
	if health.ZoneMismatch == nil {
		t.Fatalf("health did not carry the zone mismatch: %#v", health)
	}
	if health.ZoneMismatch.Stored != "UTC" || health.ZoneMismatch.Configured != "Asia/Kolkata" {
		t.Fatalf("zone mismatch = %+v", *health.ZoneMismatch)
	}
	if health.Category != "storage_time_zone" || health.Field != "storage-time-zone" {
		t.Fatalf("category/field = %q/%q", health.Category, health.Field)
	}
	for _, want := range []string{"UTC", "Asia/Kolkata"} {
		if !strings.Contains(health.Message, want) {
			t.Fatalf("health message %q does not name %q", health.Message, want)
		}
	}
}
