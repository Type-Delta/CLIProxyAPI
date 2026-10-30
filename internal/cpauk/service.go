package cpauk

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	backenddto "github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/collector"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/maintenance"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

type Reader interface {
	Summary(context.Context, model.Query) (model.Summary, error)
	Timeseries(context.Context, model.Query) (model.Timeseries, error)
	Dimensions(context.Context, model.Query) (model.DimensionPage, error)
	Events(context.Context, model.Query) (model.EventPage, error)
	Leaderboard(context.Context, model.Query) (model.LeaderboardPage, error)
	ModelLeaderboard(context.Context, model.Query) (model.ModelLeaderboard, error)
}

// ReaderV2 is additive so existing Reader implementations remain compatible
// while schema-v2 callers can discover the richer operations explicitly.
type ReaderV2 interface {
	Activity(context.Context, model.Query) (model.Activity, error)
	Analysis(context.Context, model.Query) (model.Analysis, error)
}

// EventLookup provides indexed event-detail retrieval without making the
// paginated event collection part of the lookup contract.
type EventLookup interface {
	EventByAttemptID(context.Context, string, model.Query) (model.Event, bool, error)
}

type MaintenanceRequest struct {
	Kind    string         `json:"kind"`
	Options map[string]any `json:"options,omitempty"`
}

type Maintenance interface {
	Start(context.Context, MaintenanceRequest) (model.JobStatus, error)
	Status(context.Context, string) (model.JobStatus, error)
	Cancel(context.Context, string) error
}

// Backend is the narrow contract implemented by the storage work package.
// Factory also returns the 32-byte identity key loaded with that database.
type Backend interface {
	Reader
	collector.Writer
	Close(context.Context) error
}

type BackendFactory func(context.Context, Config) (Backend, [32]byte, error)

type BackendReconfigurer interface {
	Reconfigure(context.Context, backenddto.Config) error
}

type StorageBudgetReconfigurer interface {
	ReconfigureStorageBudget(context.Context, int64, int64) error
}

type PriceBookBackend interface {
	PriceBook(context.Context) (aggregate.PriceBook, error)
	UpdatePriceBook(context.Context, aggregate.PriceBook) (aggregate.PriceBook, error)
}

type PricingSnapshotBackend interface {
	PricingSnapshot(context.Context) (backenddto.PricingSnapshot, error)
}

type PricingRefreshBackend interface {
	RefreshPricing(context.Context) (backenddto.PricingRefreshResult, error)
}

type CatalogProvider = backenddto.CatalogProvider

type CatalogProvidersBackend interface {
	CatalogProviders(context.Context) ([]CatalogProvider, time.Time, error)
}

type CatalogBindingsReconfigurer interface {
	SetCatalogBindings([]backenddto.CatalogBinding)
}

// PricingRefreshRequester is additive so discovery remains outside the core
// Service contract used by existing integrations.
type PricingRefreshRequester interface {
	RequestPricingRefresh()
}

type PricingMissingBackend interface {
	PricingMissing(context.Context, model.Range) ([]model.PricingMissing, error)
}

type ProviderCredentialBackend interface {
	ProviderCredentials(context.Context) ([]model.ProviderCredential, error)
}

type Service interface {
	Observer() coreusage.Plugin
	Reader() Reader
	Maintenance() Maintenance
	Capabilities() Capabilities
	Health() Health
	Reconfigure(Config) ReconfigureResult
	Retry(context.Context) error
	Close(context.Context) error
}

type ReconfigureResult struct {
	Applied         bool     `json:"applied"`
	RestartRequired []string `json:"restart_required,omitempty"`
	Error           error    `json:"-"`
}

// ErrTeardownUnresolved means shutdown stopped cleanup without proving that its
// storage ownership was released. A canceled finalizer never opens its barrier.
var ErrTeardownUnresolved = errors.New("analytics teardown ownership remains unresolved")

type service struct {
	factory BackendFactory

	mu                sync.RWMutex
	config            Config
	backend           Backend
	collector         *collector.Collector
	sanitizer         *collector.Sanitizer
	starting          bool
	closed            bool
	maintenanceActive bool
	validConfig       bool
	startCancel       context.CancelFunc
	startWG           sync.WaitGroup
	startupMu         sync.Mutex
	teardownDone      chan struct{}
	teardownWake      chan struct{}
	teardownFailure   error
	teardownCtx       context.Context
	teardownCancel    context.CancelFunc
	teardownExited    chan struct{}
	teardownPending   int
	closeDone         chan struct{}
	closeErr          error
	startupDone       chan struct{}
	restartRequired   atomic.Bool

	snapshots          *snapshots
	observer           *observerProxy
	reader             *readerProxy
	maint              Maintenance
	maintProxy         *maintenanceProxy
	retention          *maintenance.RetentionScheduler
	identityKey        [32]byte
	hasIdentityKey     bool
	configuredKeyIDs   []string
	rotatedKeyIDs      []string
	startSeq           atomic.Uint64
	responseCacheEpoch uint64
	pricingPending     atomic.Bool
	pricingGeneration  atomic.Uint64
	pricingNextCheck   atomic.Int64
	pricingCtx         context.Context
	pricingCancel      context.CancelFunc
	pricingWG          sync.WaitGroup
}

// LocalStoragePath reports the active database location, including when a new
// configured path is waiting for a restart.
func (s *service) LocalStoragePath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config.Path
}

func newPricingLifecycle() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

// New constructs a failure-isolated service. It never returns an analytics
// startup error to CPA. Enabled storage starts in the background.
func New(ctx context.Context, config Config, factory BackendFactory) Service {
	config = config.WithDefaults()
	if err := config.Validate(); err != nil {
		pricingCtx, pricingCancel := newPricingLifecycle()
		s := &service{
			factory:     factory,
			config:      config,
			validConfig: false,
			snapshots:   newSnapshots(config, StateCircuitOpen, "invalid_config", "Analytics is unavailable."),
			observer:    newObserverProxy(),
			maint:       unavailableMaintenance{err: &UnavailableError{Category: "invalid_config"}},
			pricingCtx:  pricingCtx, pricingCancel: pricingCancel,
		}
		s.reader = &readerProxy{service: s}
		s.maintProxy = &maintenanceProxy{service: s}
		s.observer.onUsage = s.requestPricingRefresh
		return s
	}
	state := StateDisabled
	if config.Enabled {
		state = StateStarting
	}
	pricingCtx, pricingCancel := newPricingLifecycle()
	s := &service{
		factory:     factory,
		config:      config,
		validConfig: true,
		snapshots:   newSnapshots(config, state, "", ""),
		observer:    newObserverProxy(),
		maint:       unavailableMaintenance{err: stateError(state)},
		pricingCtx:  pricingCtx, pricingCancel: pricingCancel,
	}
	s.reader = &readerProxy{service: s}
	s.maintProxy = &maintenanceProxy{service: s}
	s.observer.onUsage = s.requestPricingRefresh
	if config.Enabled {
		s.beginStart(ctx)
	}
	return s
}

func NewDisabled() Service {
	config := DefaultConfig()
	return New(context.Background(), config, nil)
}

func NewUnavailable(category string, config Config) Service {
	config = config.WithDefaults()
	config.Enabled = true
	if category == "" {
		category = "startup"
	}
	pricingCtx, pricingCancel := newPricingLifecycle()
	s := &service{
		config:      config,
		validConfig: true,
		snapshots:   newSnapshots(config, StateCircuitOpen, category, "Analytics is unavailable."),
		observer:    newObserverProxy(),
		maint:       unavailableMaintenance{err: &UnavailableError{Category: category}},
		pricingCtx:  pricingCtx, pricingCancel: pricingCancel,
	}
	s.reader = &readerProxy{service: s}
	s.maintProxy = &maintenanceProxy{service: s}
	s.observer.onUsage = s.requestPricingRefresh
	return s
}

// NewInvalid preserves the backend factory so a corrected isolated analytics
// configuration can start on hot reload without restarting CPA.
func NewInvalid(category, field string, config Config, factory BackendFactory) Service {
	config = config.WithDefaults()
	if category == "" {
		category = "invalid_config"
	}
	pricingCtx, pricingCancel := newPricingLifecycle()
	s := &service{
		factory: factory, config: config, validConfig: false,
		snapshots: newSnapshots(config, StateCircuitOpen, category, "Analytics is unavailable."),
		observer:  newObserverProxy(), maint: unavailableMaintenance{err: &UnavailableError{Category: category}},
		pricingCtx: pricingCtx, pricingCancel: pricingCancel,
	}
	s.snapshots.mutate(func(health *model.Health) { health.Field = field })
	s.reader = &readerProxy{service: s}
	s.maintProxy = &maintenanceProxy{service: s}
	s.observer.onUsage = s.requestPricingRefresh
	return s
}

func (s *service) Observer() coreusage.Plugin { return s.observer }

// ProxyResponseObserver returns the hook that completes CPA -> Client legs.
func (s *service) ProxyResponseObserver() coreusage.ProxyResponseObserver {
	return s.observer.HandleProxyResponse
}
func (s *service) Reader() Reader           { return s.reader }
func (s *service) Maintenance() Maintenance { return s.maintProxy }

func (s *service) Capabilities() Capabilities {
	value := s.snapshots.load().capabilities
	s.mu.RLock()
	if s.maintenanceActive {
		value.Available = false
	} else if value.State == StateCircuitOpen {
		value.Available = s.backend != nil && !s.closed && s.config.Enabled && !s.maintenanceActive
	}
	if s.teardownFailure != nil && s.config.Enabled && !s.closed {
		value.State = StateCircuitOpen
		value.Available = false
		value.Degraded = true
	}
	s.mu.RUnlock()
	if stats, ok := s.collectorStats(); ok {
		value.Queue.Capacity = stats.Capacity
		value.Queue.Depth = stats.Depth
		value.Queue.Dropped = stats.Dropped
	}
	return value
}

func (s *service) Health() Health {
	value := s.snapshots.load().health
	s.mu.RLock()
	backend := s.backend
	teardownFailure, enabled, closed := s.teardownFailure, s.config.Enabled, s.closed
	s.mu.RUnlock()
	if status, ok := backend.(interface {
		LegacyImportStatus() *model.LegacyImportStatus
	}); ok {
		value.LegacyImport = status.LegacyImportStatus()
	}
	if value.Category == "clickhouse_not_configured" {
		value.Message = "Analytics requires clickhouse.dsn to be configured."
	}
	if stats, ok := s.collectorStats(); ok {
		value.Queue.Capacity = stats.Capacity
		value.Queue.Depth = stats.Depth
		value.Queue.Dropped = stats.Dropped
		value.RejectedEvents = stats.Rejected
		value.TruncatedFields = stats.TruncatedFields
	}
	if s.restartRequired.Load() {
		value.Category = "restart_required"
		value.Message = "The analytics update requires a CPA restart."
	}
	if teardownFailure != nil {
		value.Category = "teardown_failure"
		value.Message = fmt.Sprintf("Analytics cleanup failed: %v. Storage ownership is retained while cleanup retries.", teardownFailure)
		if closed {
			value.Message = fmt.Sprintf("Analytics cleanup stopped: %v. Storage ownership remains unresolved after shutdown.", teardownFailure)
		}
		if enabled && !closed {
			value.State = StateCircuitOpen
		}
	}
	return value
}

func (s *service) Reconfigure(config Config) ReconfigureResult {
	config = config.WithDefaults()
	if err := config.Validate(); err != nil {
		s.snapshots.mutate(func(health *model.Health) {
			health.Category = "reconfigure_rejected"
			health.Message = "The analytics update was rejected."
		})
		return ReconfigureResult{Error: err}
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ReconfigureResult{Error: ErrClosed}
	}
	previous := s.config
	if !s.validConfig {
		s.config = config
		s.validConfig = true
		s.invalidatePricingDemandLocked()
		s.mu.Unlock()
		s.snapshots.setConfig(config)
		if config.Enabled {
			s.snapshots.mutate(func(health *model.Health) {
				health.State = StateStarting
				health.Category = ""
				health.Field = ""
				health.Message = ""
			})
			s.beginStart(context.Background())
		} else {
			s.snapshots.mutate(func(health *model.Health) {
				health.State = StateDisabled
				health.Category = ""
				health.Field = ""
				health.Message = ""
			})
		}
		return ReconfigureResult{Applied: true}
	}
	restartFields := restartRequiredFields(previous, config)
	if len(restartFields) != 0 {
		s.mu.Unlock()
		s.snapshots.mutate(func(health *model.Health) {
			health.Category = "restart_required"
			health.Message = "The analytics update requires a CPA restart."
		})
		return ReconfigureResult{RestartRequired: restartFields}
	}

	if previous.Enabled && !config.Enabled {
		oldCollector, oldBackend, oldMaintenance, oldRetention := s.collector, s.backend, s.maint, s.retention
		s.config = config
		s.validConfig = true
		s.collector, s.backend, s.sanitizer = nil, nil, nil
		s.identityKey, s.hasIdentityKey = [32]byte{}, false
		s.maintenanceActive = false
		s.maint = unavailableMaintenance{err: ErrDisabled}
		s.retention = nil
		s.invalidatePricingDemandLocked()
		s.startSeq.Add(1)
		if s.startCancel != nil {
			s.startCancel()
			s.startCancel = nil
		}
		s.starting = false
		s.observer.clear()
		s.retireGenerationLocked(oldMaintenance, oldRetention, oldCollector, oldBackend, config.ShutdownDrain)
		s.mu.Unlock()
		s.snapshots.setConfig(config)
		s.snapshots.mutate(func(health *model.Health) {
			health.State = StateDisabled
			health.Category = ""
			health.Field = ""
			health.Message = ""
		})
		return ReconfigureResult{Applied: true}
	}

	if !previous.Enabled && config.Enabled {
		s.config = config
		s.invalidatePricingDemandLocked()
		s.mu.Unlock()
		s.snapshots.setConfig(config)
		s.snapshots.mutate(func(health *model.Health) {
			health.State = StateStarting
			health.Category = ""
			health.Field = ""
			health.Message = ""
		})
		s.beginStart(context.Background())
		return ReconfigureResult{Applied: true}
	}

	backend := s.backend
	currentCollector := s.collector
	currentRetention := s.retention
	if config.Enabled && backend == nil && currentCollector == nil {
		if s.startCancel != nil {
			s.startCancel()
			s.startCancel = nil
		}
		s.startSeq.Add(1)
		s.starting = false
		s.config = config
		s.invalidatePricingDemandLocked()
		s.mu.Unlock()
		s.snapshots.setConfig(config)
		s.snapshots.mutate(func(health *model.Health) {
			health.State = StateStarting
			health.Category = ""
			health.Field = ""
			health.Message = ""
		})
		s.beginStart(context.Background())
		return ReconfigureResult{Applied: true}
	}
	s.mu.Unlock()
	if previous.MaxStorageBytes != config.MaxStorageBytes || previous.MinFreeBytes != config.MinFreeBytes {
		reconfigurer, ok := backend.(StorageBudgetReconfigurer)
		if !ok {
			return ReconfigureResult{Error: ErrUnavailable}
		}
		if err := safeStorageBudgetReconfigure(reconfigurer, config); err != nil {
			s.snapshots.mutate(func(health *model.Health) {
				health.Category = "reconfigure_rejected"
				health.Message = "The analytics update was rejected."
			})
			return ReconfigureResult{Error: err}
		}
	}
	if reconfigurer, ok := backend.(BackendReconfigurer); ok {
		if err := safeBackendReconfigure(reconfigurer, config); err != nil {
			s.snapshots.mutate(func(health *model.Health) {
				health.Category = "reconfigure_rejected"
				health.Message = "The analytics update was rejected."
			})
			return ReconfigureResult{Error: err}
		}
	}
	if currentRetention != nil && previous.HotRetentionDays != config.HotRetentionDays {
		if err := currentRetention.Reconfigure(config.HotRetentionDays); err != nil {
			return ReconfigureResult{Error: err}
		}
	}
	if currentCollector != nil {
		if err := currentCollector.Reconfigure(config.BatchSize, config.FlushInterval, config.CircuitFailureThreshold); err != nil {
			return ReconfigureResult{Error: err}
		}
	}
	if !catalogBindingsEqual(previous.CatalogBindings, config.CatalogBindings) {
		if reconfigurer, ok := backend.(CatalogBindingsReconfigurer); ok {

			reconfigurer.SetCatalogBindings(config.CatalogBindings)
		}
	}
	s.mu.Lock()
	s.config = config
	s.invalidatePricingDemandLocked()
	s.mu.Unlock()
	s.snapshots.setConfig(config)
	return ReconfigureResult{Applied: true}
}

func (s *service) Retry(ctx context.Context) error {
	s.mu.Lock()
	s.invalidatePricingDemandLocked()
	currentCollector := s.collector
	currentRetention := s.retention
	maintenanceActive := s.maintenanceActive
	config := s.config
	closed := s.closed
	if s.teardownWake != nil {
		select {
		case s.teardownWake <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if currentCollector == nil {
		if config.Enabled {
			s.beginStart(context.Background())
			return nil
		}
		return ErrDisabled
	}
	if currentCollector.Retry() {
		return nil
	}
	if currentCollector.Ready() || maintenanceActive {
		if currentRetention != nil {
			return currentRetention.Reconfigure(config.HotRetentionDays)
		}
		return nil
	}
	return ErrUnavailable
}

// PriceBook exposes durable pricing through an optional service facade without
// adding storage concerns to the core Service contract.
func (s *service) PriceBook(ctx context.Context) (book aggregate.PriceBook, err error) {
	backend, err := s.backendForRead()
	if err != nil {
		return book, err
	}
	pricing, ok := backend.(PriceBookBackend)
	if !ok {
		return book, ErrUnavailable
	}
	defer func() {
		if recover() != nil {
			book = aggregate.PriceBook{}
			err = ErrInternal
		}
	}()
	return pricing.PriceBook(ctx)
}

// UpdatePriceBook replaces the durable pricing catalog atomically. Existing
// events keep their stored ingestion cost while future writes use the new book.
func (s *service) UpdatePriceBook(ctx context.Context, book aggregate.PriceBook) (updated aggregate.PriceBook, err error) {
	backend, err := s.backendForRead()
	if err != nil {
		return updated, err
	}
	pricing, ok := backend.(PriceBookBackend)
	if !ok {
		return updated, ErrUnavailable
	}
	defer func() {
		if recover() != nil {
			updated = aggregate.PriceBook{}
			err = ErrInternal
		}
	}()
	updated, err = pricing.UpdatePriceBook(ctx, book)
	if err == nil {
		s.mu.Lock()
		s.responseCacheEpoch++
		s.mu.Unlock()
	}
	return updated, err
}

func (s *service) PricingSnapshot(ctx context.Context) (snapshot backenddto.PricingSnapshot, err error) {
	backend, err := s.backendForRead()
	if err != nil {
		return snapshot, err
	}
	pricing, ok := backend.(PricingSnapshotBackend)
	if !ok {
		return snapshot, ErrUnavailable
	}
	defer func() {
		if recover() != nil {
			snapshot = backenddto.PricingSnapshot{}
			err = ErrInternal
		}
	}()
	return pricing.PricingSnapshot(ctx)
}

func (s *service) CatalogProviders(ctx context.Context) (providers []backenddto.CatalogProvider, updatedAt time.Time, err error) {
	backend, err := s.backendForRead()
	if err != nil {
		return nil, time.Time{}, err
	}
	providerStore, ok := backend.(CatalogProvidersBackend)
	if !ok {
		return nil, time.Time{}, ErrUnavailable
	}
	defer func() {
		if recover() != nil {
			providers = nil
			updatedAt = time.Time{}
			err = ErrInternal
		}
	}()
	return providerStore.CatalogProviders(ctx)
}

// RefreshPricing performs one demand-driven refresh for callers that can wait
// for a result, such as tests or an explicit administrative action.
func (s *service) RefreshPricing(ctx context.Context) (result backenddto.PricingRefreshResult, err error) {
	backend, err := s.backendForRead()
	if err != nil {
		return result, err
	}
	pricing, ok := backend.(PricingRefreshBackend)
	if !ok {
		return result, ErrUnavailable
	}
	generation := s.pricingGeneration.Load()
	result, err = safePricingRefresh(pricing, ctx)
	s.recordPricingRefresh(generation, result, err, false)
	return result, err
}

// RequestPricingRefresh starts a coalesced background demand. The caller is
// never held up by models.dev I/O, and the service lifecycle context cancels
// an in-flight request during shutdown.
func (s *service) RequestPricingRefresh() {
	if s == nil {
		return
	}
	s.mu.RLock()
	if s.closed || !s.config.Enabled || s.backend == nil {
		s.mu.RUnlock()
		return
	}
	if !s.pricingDemandDue(time.Now()) {
		s.mu.RUnlock()
		return
	}
	pricing, ok := s.backend.(PricingRefreshBackend)
	if !ok || !s.pricingPending.CompareAndSwap(false, true) {
		s.mu.RUnlock()
		return
	}
	generation := s.pricingGeneration.Load()
	ctx := s.pricingCtx
	if ctx == nil {
		ctx = context.Background()
	}
	s.pricingWG.Add(1)
	s.mu.RUnlock()
	go func() {
		defer s.pricingWG.Done()
		result, err := safePricingRefresh(pricing, ctx)
		s.recordPricingRefresh(generation, result, err, true)
	}()
}

func (s *service) requestPricingRefresh() {
	s.RequestPricingRefresh()
}

func safePricingRefresh(pricing PricingRefreshBackend, ctx context.Context) (result backenddto.PricingRefreshResult, err error) {
	defer func() {
		if recover() != nil {
			result = backenddto.PricingRefreshResult{Err: ErrInternal}
			err = ErrInternal
		}
	}()
	return pricing.RefreshPricing(ctx)
}

func (s *service) pricingDemandDue(now time.Time) bool {
	next := s.pricingNextCheck.Load()
	return next == 0 || now.UnixNano() >= next
}

func (s *service) recordPricingRefresh(generation uint64, result backenddto.PricingRefreshResult, err error, clearPending bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.pricingGeneration.Load() != generation {
		return
	}
	if result.Attempted && err == nil && result.Err == nil {
		s.responseCacheEpoch++
	}
	next := pricingRefreshNextCheck(result, err, time.Now())
	if next.IsZero() {
		s.pricingNextCheck.Store(0)
	} else {
		s.pricingNextCheck.Store(next.UnixNano())
	}
	if clearPending {
		s.pricingPending.Store(false)
	}
}

func pricingRefreshNextCheck(result backenddto.PricingRefreshResult, err error, now time.Time) time.Time {
	if retryAt := result.Snapshot.Provenance.CatalogRetryAt; retryAt.After(now) {
		return retryAt
	}
	if err == nil && result.Err == nil {
		if expiresAt := result.Snapshot.Provenance.CatalogExpiresAt; expiresAt.After(now) {
			return expiresAt
		}
		return time.Time{}
	}
	return now.Add(backenddto.PricingRetrySuppression)
}

func (s *service) invalidatePricingDemandLocked() {
	s.pricingGeneration.Add(1)
	s.pricingNextCheck.Store(0)
	s.pricingPending.Store(false)
}

// MarkRestartRequired lets the CPA integration surface restart-only settings
// that intentionally remain outside the CPAUK configuration boundary.
func (s *service) MarkRestartRequired(_ []string) {
	s.restartRequired.Store(true)
	if s == nil || s.snapshots == nil {
		return
	}
	s.snapshots.mutate(func(health *model.Health) {
		health.Category = "restart_required"
		health.Field = ""
		health.Message = "The analytics update requires a CPA restart."
	})
}

func (s *service) MarkConfigProblem(category, field string) {
	if s == nil || s.snapshots == nil {
		return
	}
	s.snapshots.mutate(func(health *model.Health) {
		health.Category = category
		health.Field = field
		health.Message = "The analytics update was rejected."
	})
}

func (s *service) CredentialID(provider, authIndex, authID string) (*string, error) {
	s.mu.RLock()
	identityKey := s.identityKey
	hasIdentityKey := s.hasIdentityKey
	storeCredentialID := s.config.Privacy.StoreCredentialID
	s.mu.RUnlock()
	if !storeCredentialID {
		return nil, nil
	}
	if !hasIdentityKey {
		return nil, ErrUnavailable
	}
	return model.CredentialID(identityKey[:], provider, authIndex, authID)
}

func (s *service) ReplaceProviderQuotaSnapshots(ctx context.Context, snapshots []backenddto.ProviderQuotaSnapshot) (err error) {
	s.mu.RLock()
	storeCredentialID := s.config.Privacy.StoreCredentialID
	s.mu.RUnlock()
	if !storeCredentialID {
		snapshots = nil
	}
	backend, err := s.backendForRead()
	if err != nil {
		return err
	}
	provider, ok := backend.(backenddto.ProviderQuotaStore)
	if !ok {
		return ErrUnavailable
	}
	defer func() {
		if recover() != nil {
			err = ErrInternal
		}
	}()
	return provider.ReplaceProviderQuotaSnapshots(ctx, snapshots)
}

func (s *service) ProviderQuotaSnapshots(ctx context.Context) (snapshots []backenddto.ProviderQuotaSnapshot, err error) {
	s.mu.RLock()
	storeCredentialID := s.config.Privacy.StoreCredentialID
	s.mu.RUnlock()
	if !storeCredentialID {
		return []backenddto.ProviderQuotaSnapshot{}, nil
	}
	backend, err := s.backendForRead()
	if err != nil {
		return nil, err
	}
	provider, ok := backend.(backenddto.ProviderQuotaStore)
	if !ok {
		return nil, ErrUnavailable
	}
	defer func() {
		if recover() != nil {
			snapshots = nil
			err = ErrInternal
		}
	}()
	return provider.ProviderQuotaSnapshots(ctx)
}

func (s *service) PricingMissing(ctx context.Context, selected model.Range) (result []model.PricingMissing, err error) {
	backend, err := s.backendForRead()
	if err != nil {
		return nil, err
	}
	provider, ok := backend.(PricingMissingBackend)
	if !ok {
		return nil, ErrUnavailable
	}
	err = recoverQuery(func() error { result, err = provider.PricingMissing(ctx, selected); return err })
	s.reader.recordQueryFailure(err)
	return result, err
}

func (s *service) ProviderCredentials(ctx context.Context) (result []model.ProviderCredential, err error) {
	s.mu.RLock()
	storeCredentialID := s.config.Privacy.StoreCredentialID
	s.mu.RUnlock()
	if !storeCredentialID {
		return []model.ProviderCredential{}, nil
	}
	backend, err := s.backendForRead()
	if err != nil {
		return nil, err
	}
	provider, ok := backend.(ProviderCredentialBackend)
	if !ok {
		return nil, ErrUnavailable
	}
	err = recoverQuery(func() error { result, err = provider.ProviderCredentials(ctx); return err })
	s.reader.recordQueryFailure(err)
	return result, err
}

func (s *service) KeyCatalog(ctx context.Context, query model.Query) (page backenddto.KeyCatalogPage, err error) {
	backend, err := s.backendForRead()
	if err != nil {
		return page, err
	}
	provider, ok := backend.(interface {
		KeyCatalog(context.Context, model.Query) (backenddto.KeyCatalogPage, error)
	})
	if !ok {
		return page, ErrUnavailable
	}
	defer func() {
		if recover() != nil {
			page = backenddto.KeyCatalogPage{}
			err = ErrInternal
		}
	}()
	return provider.KeyCatalog(ctx, query)
}

func (s *service) UpdateKeyLifecycle(ctx context.Context, configuredIDs, rotatedIDs []string) (err error) {
	s.mu.Lock()
	s.configuredKeyIDs = append([]string(nil), configuredIDs...)
	s.rotatedKeyIDs = append([]string(nil), rotatedIDs...)
	backend := s.backend
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if backend == nil {
		return nil
	}
	provider, ok := backend.(interface {
		UpdateKeyLifecycle(context.Context, []string, []string) error
	})
	if !ok {
		return ErrUnavailable
	}
	defer func() {
		if recover() != nil {
			err = ErrInternal
		}
	}()
	err = provider.UpdateKeyLifecycle(ctx, configuredIDs, rotatedIDs)
	if err == nil {
		s.mu.Lock()
		s.responseCacheEpoch++
		s.mu.Unlock()
	}
	return err
}

func (s *service) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	drain := s.config.ShutdownDrain
	if drain <= 0 || drain > time.Minute {
		drain = DefaultShutdownDrain
	}
	drainCtx, cancel := context.WithTimeout(ctx, drain)
	defer cancel()
	if s.closed {
		done := s.closeDone
		s.mu.Unlock()
		if done != nil {
			select {
			case <-done:
			case <-drainCtx.Done():
				return errors.Join(ErrTeardownUnresolved, drainCtx.Err())
			}
		}
		s.mu.RLock()
		resourceErr := s.closeErr
		s.mu.RUnlock()
		return errors.Join(resourceErr, s.joinTeardowns(drainCtx))
	}
	s.closeDone = make(chan struct{})
	closeDone := s.closeDone
	defer close(closeDone)
	s.startupDone = make(chan struct{})
	startupDone := s.startupDone
	if s.teardownCancel != nil {
		s.teardownCancel()
	}

	s.closed = true
	s.invalidatePricingDemandLocked()
	s.startSeq.Add(1)
	if s.startCancel != nil {
		s.startCancel()
		s.startCancel = nil
	}
	if s.pricingCancel != nil {
		s.pricingCancel()
		s.pricingCancel = nil
	}
	currentCollector, backend, currentMaintenance, currentRetention := s.collector, s.backend, s.maint, s.retention
	s.collector, s.backend, s.sanitizer = nil, nil, nil
	s.identityKey, s.hasIdentityKey = [32]byte{}, false
	s.maintenanceActive = false
	s.maint = unavailableMaintenance{err: ErrClosed}
	s.retention = nil
	s.observer.clear()
	s.mu.Unlock()
	go func() { s.startWG.Wait(); close(startupDone) }()
	if worker, ok := backend.(interface{ StopBackground() }); ok {
		worker.StopBackground()
	}
	// Stop demand refreshes before closing the backend they may be reading.
	s.pricingWG.Wait()
	s.snapshots.mutate(func(health *model.Health) { health.State = StateStopping })

	var errs []error
	if currentRetention != nil {
		if err := currentRetention.Close(drainCtx); err != nil {
			errs = append(errs, fmt.Errorf("close analytics retention scheduler: %w", err))
		}
	}
	if closer, ok := currentMaintenance.(interface{ Close(context.Context) error }); ok {
		if err := closer.Close(drainCtx); err != nil {
			errs = append(errs, fmt.Errorf("close analytics maintenance: %w", err))
		}
	}
	if currentCollector != nil {
		if err := currentCollector.Close(drainCtx); err != nil {
			errs = append(errs, fmt.Errorf("close analytics collector: %w", err))
		}
	}
	if backend != nil {
		if err := safeBackendClose(drainCtx, backend); err != nil {
			errs = append(errs, fmt.Errorf("close analytics backend: %w", err))
		}
	}

	resourceErr := errors.Join(errs...)
	s.mu.Lock()
	s.closeErr = resourceErr
	s.mu.Unlock()
	return errors.Join(resourceErr, s.joinTeardowns(drainCtx))
}

// Joining finalizer goroutines is distinct from certifying ownership release.
func (s *service) joinTeardowns(ctx context.Context) error {
	s.mu.RLock()
	startupDone := s.startupDone
	s.mu.RUnlock()
	if startupDone != nil {
		select {
		case <-startupDone:
		case <-ctx.Done():
			return errors.Join(ErrTeardownUnresolved, ctx.Err())
		}
	}
	s.mu.RLock()
	exited := s.teardownExited
	s.mu.RUnlock()
	if exited != nil {
		select {
		case <-exited:
		case <-ctx.Done():
			return errors.Join(ErrTeardownUnresolved, ctx.Err())
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.teardownDone == nil {
		return nil
	}
	select {
	case <-s.teardownDone:
		return nil
	default:
	}
	if s.teardownFailure == nil {
		s.teardownFailure = ErrTeardownUnresolved
	}
	return errors.Join(ErrTeardownUnresolved, s.teardownFailure)
}

func (s *service) beginStart(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.closed || s.starting || s.backend != nil || s.collector != nil || !s.config.Enabled {
		s.mu.Unlock()
		return
	}
	s.starting = true
	config := s.config
	startCtx, cancel := context.WithCancel(ctx)
	s.startCancel = cancel
	sequence := s.startSeq.Add(1)
	s.startWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.startWG.Done()
		s.startupMu.Lock()
		defer s.startupMu.Unlock()
		s.mu.RLock()
		previousTeardown := s.teardownDone
		s.mu.RUnlock()
		if previousTeardown != nil {
			select {
			case <-previousTeardown:
			case <-startCtx.Done():
				return
			}
		}
		if startCtx.Err() == nil {
			s.start(startCtx, sequence, config)
		}
	}()
}

func (s *service) start(ctx context.Context, sequence uint64, config Config) {
	var backend Backend
	var identityKey [32]byte
	var err error
	delay := 100 * time.Millisecond
	for {
		backend, identityKey, err = callFactory(ctx, s.factory, config)
		if err == nil {
			break
		}
		var mismatch backenddto.ZoneMismatchError
		if errors.As(err, &mismatch) || errors.Is(err, backenddto.ErrClickHouseNotConfigured) || errors.Is(err, ErrInternal) {
			break
		}
		var permanent interface{ Permanent() bool }
		if errors.As(err, &permanent) && permanent.Permanent() {
			break
		}
		s.snapshots.mutate(func(h *model.Health) {
			h.State = StateCircuitOpen
			h.Category = startupErrorCategory(err)
			h.Message = "Analytics is unavailable."
		})
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(delay*2, 30*time.Second)
	}
	if err != nil {
		var mismatch backenddto.ZoneMismatchError
		if errors.As(err, &mismatch) {
			s.finishStartZoneMismatch(sequence, mismatch)
			return
		}
		s.finishStartFailure(sequence, startupErrorCategory(err))
		return
	}
	sanitizer := collector.NewSanitizer(collector.SanitizerOptions{
		IdentityKey:     identityKey,
		StoreCredential: config.Privacy.StoreCredentialID,
	})
	callbacks := s.collectorCallbacks(sequence)
	currentCollector, err := collector.New(backend, collector.Options{
		Capacity:         config.QueueCapacity,
		BatchSize:        config.BatchSize,
		FlushInterval:    config.FlushInterval,
		FailureThreshold: config.CircuitFailureThreshold,
		Callbacks:        callbacks,
	})
	if err != nil {
		s.retireGeneration(nil, backend, config.ShutdownDrain)
		s.finishStartFailure(sequence, "collector_start")
		return
	}

	s.mu.Lock()
	if s.closed || sequence != s.startSeq.Load() || !s.config.Enabled {
		s.mu.Unlock()
		s.retireGeneration(currentCollector, backend, config.ShutdownDrain)
		return
	}
	if lifecycle, ok := backend.(interface {
		UpdateKeyLifecycle(context.Context, []string, []string) error
	}); ok {
		configuredKeyIDs := append([]string(nil), s.configuredKeyIDs...)
		rotatedKeyIDs := append([]string(nil), s.rotatedKeyIDs...)
		if err = safeKeyLifecycleUpdate(ctx, lifecycle, configuredKeyIDs, rotatedKeyIDs); err != nil {
			s.mu.Unlock()
			s.retireGeneration(currentCollector, backend, config.ShutdownDrain)
			s.finishStartFailure(sequence, "key_lifecycle")
			return
		}
	}
	s.invalidatePricingDemandLocked()
	s.backend = backend
	s.collector = currentCollector
	s.sanitizer = sanitizer
	s.identityKey = identityKey
	s.hasIdentityKey = true
	var retention *maintenance.RetentionScheduler
	if database, ok := backend.(maintenance.Store); ok {
		var stateBeforeMaintenance model.Health
		controller := maintenance.New(maintenance.Hooks{
			Detach: func(ctx context.Context) error {
				if err := s.maintenanceReady(ctx); err != nil {
					return err
				}
				s.mu.Lock()
				s.maintenanceActive = true
				s.responseCacheEpoch++
				s.mu.Unlock()
				stateBeforeMaintenance = s.Health()
				s.snapshots.mutate(func(health *model.Health) {
					health.State = StateDegraded
					health.Category = "maintenance"
					health.Message = "Analytics maintenance is in progress."
				})
				s.observer.clear()
				currentCollector.Detach()
				return nil
			},
			Drain: func(ctx context.Context) error {
				if err := currentCollector.Drain(ctx); err != nil {
					return err
				}
				return nil
			},
			Attach: func(context.Context) error {
				identityProvider, okIdentity := backend.(interface{ IdentityKeyArray() [32]byte })
				if !okIdentity {
					return ErrUnavailable
				}
				identityKey := identityProvider.IdentityKeyArray()
				s.mu.Lock()
				if s.closed || s.backend != backend || s.collector != currentCollector {
					s.mu.Unlock()
					return ErrClosed
				}
				sanitizer := collector.NewSanitizer(collector.SanitizerOptions{
					IdentityKey: identityKey, StoreCredential: s.config.Privacy.StoreCredentialID,
				})
				s.identityKey = identityKey
				s.hasIdentityKey = true
				s.sanitizer = sanitizer
				s.mu.Unlock()
				if !currentCollector.Resume() {
					return ErrUnavailable
				}
				s.mu.Lock()
				s.maintenanceActive = false
				s.mu.Unlock()
				s.snapshots.mutate(func(health *model.Health) {
					if health.Category == "maintenance" {
						health.State = stateBeforeMaintenance.State
						health.Category = stateBeforeMaintenance.Category
						health.Field = stateBeforeMaintenance.Field
						health.Message = stateBeforeMaintenance.Message
					}
				})
				return nil
			},
		}, maintenance.StoreOperations(database))
		s.maint = maintenanceAdapter{controller: controller}
		retention, err = maintenance.NewRetentionScheduler(database, config.HotRetentionDays)
		if err != nil {
			s.mu.Unlock()
			_ = controller.Close(context.Background())
			s.retireGeneration(currentCollector, backend, config.ShutdownDrain)
			s.finishStartFailure(sequence, "retention_start")
			return
		}
		if err = retention.SetRunner(func(runContext context.Context, rawCutoff, hourlyCutoff time.Time, _ int) (backenddto.RetentionResult, error) {
			// Wait before scheduling maintenance so startup import never detaches intake.
			if importer, ok := backend.(interface{ WaitLegacyImport(context.Context) error }); ok {
				if err := importer.WaitLegacyImport(runContext); err != nil {
					return backenddto.RetentionResult{}, err
				}
			}
			result, runErr := runScheduledRetention(runContext, controller, rawCutoff, hourlyCutoff)
			if errors.Is(runErr, maintenance.ErrJobRunning) {
				return backenddto.RetentionResult{}, nil
			}
			if runErr != nil {
				s.snapshots.mutate(func(health *model.Health) {
					health.State = StateDegraded
					health.Category = "retention"
					health.Message = "Analytics retention is unavailable."
				})
				return result, runErr
			}
			s.snapshots.mutate(func(health *model.Health) {
				if health.Category == "retention" || health.Category == "retention_start" {
					health.State = StateReady
					health.Category = ""
					health.Message = ""
				}
				health.RetentionCutoff = cloneTime(&rawCutoff)
			})
			return result, nil
		}); err != nil {
			s.mu.Unlock()
			_ = controller.Close(context.Background())
			s.retireGeneration(currentCollector, backend, config.ShutdownDrain)
			s.finishStartFailure(sequence, "retention_start")
			return
		}
		s.retention = retention
	} else {
		s.maint = unavailableMaintenance{err: ErrUnavailable}
	}
	s.starting = false
	if s.startCancel != nil {
		s.startCancel()
	}
	s.startCancel = nil
	s.mu.Unlock()
	currentCollector.Start()
	s.snapshots.mutate(func(health *model.Health) {
		health.State = StateReady
		health.Category = ""
		health.Field = ""
		health.Message = ""
		health.ZoneMismatch = nil
	})
	if retention != nil {
		if err = retention.Start(context.Background()); err != nil {
			s.snapshots.mutate(func(health *model.Health) {
				health.State = StateDegraded
				health.Category = "retention_start"
				health.Message = "Analytics retention is unavailable."
			})
		}
	}
}

func (s *service) finishStartFailure(sequence uint64, category string) {
	s.mu.Lock()
	if sequence != s.startSeq.Load() || s.closed {
		s.mu.Unlock()
		return
	}
	s.starting = false
	s.startCancel = nil
	s.mu.Unlock()
	s.snapshots.mutate(func(health *model.Health) {
		health.State = StateCircuitOpen
		health.Category = category
		health.Message = "Analytics is unavailable."
		health.ZoneMismatch = nil
	})
}

// finishStartZoneMismatch names both storage time zones in health so the
// operator can see why the store refused to open instead of a bare
// "analytics unavailable".
func (s *service) finishStartZoneMismatch(sequence uint64, mismatch backenddto.ZoneMismatchError) {
	s.mu.Lock()
	if sequence != s.startSeq.Load() || s.closed {
		s.mu.Unlock()
		return
	}
	s.starting = false
	s.startCancel = nil
	s.mu.Unlock()
	s.snapshots.mutate(func(health *model.Health) {
		health.State = StateCircuitOpen
		health.Category = mismatch.Category()
		health.Field = "storage-time-zone"
		health.Message = "Analytics storage is pinned to time zone " + mismatch.Stored + " but the configuration requests " + mismatch.Configured + ". Restore the stored zone or reset analytics storage."
		health.ZoneMismatch = &model.ZoneMismatch{Stored: mismatch.Stored, Configured: mismatch.Configured}
	})
}

func (s *service) collectorCallbacks(sequence uint64) collector.Callbacks {
	return collector.Callbacks{
		Queue: func(depth, dropped int64) {
			s.snapshots.mutate(func(health *model.Health) {
				health.Queue.Depth = depth
				health.Queue.Dropped = dropped
			})
		},
		State: func(state model.AnalyticsState, category string) {
			s.snapshots.mutate(func(health *model.Health) {
				health.State = state
				health.Category = category
			})
		},
		Wrote: func(at time.Time) {
			s.snapshots.mutate(func(health *model.Health) { health.LastSuccessfulWrite = cloneTime(&at) })
		},
		Panic: func(category string, at time.Time, count int) {
			s.snapshots.mutate(func(health *model.Health) {
				health.LastPanicCategory = category
				health.LastPanicAt = cloneTime(&at)
				health.RestartCount = count
			})
		},
		Abandoned: func(count int64) {
			s.snapshots.mutate(func(health *model.Health) { health.AbandonedEvents += count })
		},
		Generation: func(generation uint64) {
			if sequence != s.startSeq.Load() {
				return
			}
			s.mu.RLock()
			currentCollector := s.collector
			sanitizer := s.sanitizer
			s.mu.RUnlock()
			if currentCollector != nil && sanitizer != nil && currentCollector.Generation() == generation {
				s.observer.set(collector.NewAdapter(currentCollector, sanitizer))
			}
		},
	}
}

func (s *service) backendForRead() (Backend, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.backendForReadLocked()
}

// AnalyticsResponseCacheEpoch invalidates short-lived HTTP snapshots across
// service, configuration, and maintenance changes. Intake writes deliberately
// leave the epoch unchanged so callers can share a bounded-staleness snapshot.
func (s *service) AnalyticsResponseCacheEpoch() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, err := s.backendForReadLocked(); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%d", s.startSeq.Load(), s.pricingGeneration.Load(), s.responseCacheEpoch), nil
}

func (s *service) backendForReadLocked() (Backend, error) {
	if s.closed {
		return nil, ErrClosed
	}
	if !s.config.Enabled {
		return nil, ErrDisabled
	}
	if s.maintenanceActive {
		return nil, ErrMaintenance
	}
	if s.backend == nil {
		return nil, ErrUnavailable
	}
	return s.backend, nil
}

func (s *service) collectorStats() (collector.Stats, bool) {
	s.mu.RLock()
	currentCollector := s.collector
	s.mu.RUnlock()
	if currentCollector == nil {
		return collector.Stats{}, false
	}
	return currentCollector.Stats(), true
}

func restartRequiredFields(previous, next Config) []string {
	fields := make([]string, 0, 3)
	if previous.Path != next.Path {
		fields = append(fields, "path")
	}
	if previous.QueueCapacity != next.QueueCapacity {
		fields = append(fields, "queue-capacity")
	}
	if previous.Privacy != next.Privacy {
		fields = append(fields, "privacy.store-credential-id")
	}
	return fields
}

func catalogBindingsEqual(left, right []CatalogBinding) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func callFactory(ctx context.Context, factory BackendFactory, config Config) (backend Backend, key [32]byte, err error) {
	if factory == nil {
		return nil, key, ErrUnavailable
	}
	defer func() {
		if recover() != nil {
			backend = nil
			err = ErrInternal
		}
	}()
	return factory(ctx, config)
}

func (s *service) retireGeneration(current *collector.Collector, backend Backend, timeout time.Duration) {
	s.mu.Lock()
	s.retireGenerationLocked(nil, nil, current, backend, timeout)
	s.mu.Unlock()
}

// Reload retirement and unpublished startup owners enter the same queue.
func (s *service) retireGenerationLocked(currentMaintenance Maintenance, retention *maintenance.RetentionScheduler, current *collector.Collector, backend Backend, timeout time.Duration) {
	previous := s.teardownDone
	done := make(chan struct{})
	s.teardownDone = done
	if s.teardownWake == nil {
		s.teardownWake = make(chan struct{}, 1)
	}
	wake := s.teardownWake
	if s.teardownCtx == nil {
		s.teardownCtx, s.teardownCancel = context.WithCancel(context.Background())
		if s.closed {
			s.teardownCancel()
		}
	}
	ctx := s.teardownCtx
	if s.teardownPending == 0 {
		s.teardownExited = make(chan struct{})
	}
	s.teardownPending++
	go func() {
		released := false
		defer func() {
			// A factory may return its owner after terminal shutdown has already begun.
			// Cancel its workers even if this finalizer never reached its predecessor.
			// The Closes get the canceled terminal context, so they stop without draining.
			if !released && ctx.Err() != nil {
				if worker, ok := backend.(interface{ StopBackground() }); ok {
					worker.StopBackground()
				}
				if retention != nil {
					_ = retention.Close(ctx)
				}
				if closer, ok := currentMaintenance.(interface{ Close(context.Context) error }); ok {
					_ = closer.Close(ctx)
				}
				if current != nil {
					_ = current.Close(ctx)
				}
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if !released && s.teardownFailure == nil {
				s.teardownFailure = ErrTeardownUnresolved
			}
			s.teardownPending--
			if s.teardownPending == 0 {
				close(s.teardownExited)
			}
		}()
		if previous != nil {
			select {
			case <-previous:
			case <-ctx.Done():
				return
			}
		}
		delay := 100 * time.Millisecond
		for {
			if ctx.Err() != nil {
				return
			}
			err := closeServiceGeneration(ctx, currentMaintenance, retention, current, backend, timeout)
			s.mu.Lock()
			if err == nil {
				if !s.closed || s.teardownDone == done {
					s.teardownFailure = nil
				}
			} else if ctx.Err() == nil || s.teardownFailure == nil {
				s.teardownFailure = err
			}
			s.mu.Unlock()
			if err == nil {
				released = true
				close(done)
				return
			}
			if ctx.Err() != nil {
				return
			}
			log.WithError(err).Warn("analytics teardown failed; retaining storage ownership and retrying cleanup")
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-wake:
			case <-ctx.Done():
				timer.Stop()
				return
			}
			timer.Stop()
			delay = min(delay*2, 30*time.Second)
		}
	}()
}

// The drain deadline bounds queued work, never ownership. Joining the canceled
// workers and closing the backend run to completion before a successor may open.
func closeServiceGeneration(lifetime context.Context, currentMaintenance Maintenance, retention *maintenance.RetentionScheduler, currentCollector *collector.Collector, backend Backend, timeout time.Duration) error {
	if worker, ok := backend.(interface{ StopBackground() }); ok {
		worker.StopBackground()
	}
	ctx, cancel := context.WithTimeout(lifetime, timeout)
	defer cancel()
	var errs []error
	if retention != nil {
		if err := retention.Close(lifetime); err != nil {
			if lifetime.Err() == nil {
				return err
			}
			errs = append(errs, err)
		}
	}
	if closer, ok := currentMaintenance.(interface{ Close(context.Context) error }); ok {
		if err := closer.Close(lifetime); err != nil {
			if lifetime.Err() == nil {
				return err
			}
			errs = append(errs, err)
		}
	}
	if currentCollector != nil {
		_ = currentCollector.Close(ctx)
		if err := currentCollector.Close(lifetime); err != nil {
			if lifetime.Err() == nil {
				return err
			}
			errs = append(errs, err)
		}
	}
	if backend != nil {
		if err := safeBackendClose(lifetime, backend); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func safeBackendClose(ctx context.Context, backend Backend) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrInternal
		}
	}()
	return backend.Close(ctx)
}

func safeBackendReconfigure(backend BackendReconfigurer, config Config) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrInternal
		}
	}()
	return backend.Reconfigure(context.Background(), backenddto.Config{
		Path: config.Path, MaxStorageBytes: config.MaxStorageBytes, MinFreeBytes: config.MinFreeBytes,
		RetentionTimeZone: config.StorageTimeZone, CatalogBindings: config.CatalogBindings,
	})
}

func safeStorageBudgetReconfigure(backend StorageBudgetReconfigurer, config Config) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrInternal
		}
	}()
	return backend.ReconfigureStorageBudget(context.Background(), config.MaxStorageBytes, config.MinFreeBytes)
}

func safeKeyLifecycleUpdate(ctx context.Context, backend interface {
	UpdateKeyLifecycle(context.Context, []string, []string) error
}, configuredKeyIDs, rotatedKeyIDs []string) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrInternal
		}
	}()
	return backend.UpdateKeyLifecycle(ctx, configuredKeyIDs, rotatedKeyIDs)
}

func runScheduledRetention(ctx context.Context, controller *maintenance.Controller, rawCutoff, hourlyCutoff time.Time) (backenddto.RetentionResult, error) {
	job, err := controller.Start(ctx, maintenance.Request{Kind: "retention", Options: map[string]any{
		"raw_cutoff": rawCutoff.Format(time.RFC3339Nano), "hourly_cutoff": hourlyCutoff.Format(time.RFC3339Nano),
	}})
	if err != nil {
		return backenddto.RetentionResult{}, err
	}
	status, err := controller.Await(ctx, job.JobID)
	if err != nil {
		return backenddto.RetentionResult{}, err
	}
	if status.State != model.JobSucceeded {
		return backenddto.RetentionResult{}, fmt.Errorf("scheduled analytics retention failed")
	}
	return backenddto.RetentionResult{
		Cutoff: rawCutoff, HourlyCutoff: hourlyCutoff,
		RolledUpRows:      maintenanceResultInt64(status.Result, "rolled_up_rows"),
		DeletedRows:       maintenanceResultInt64(status.Result, "deleted_rows"),
		DailyRolledUpRows: maintenanceResultInt64(status.Result, "daily_rolled_up_rows"),
		DeletedHourlyRows: maintenanceResultInt64(status.Result, "deleted_hourly_rows"),
	}, nil
}

func maintenanceResultInt64(result map[string]any, name string) int64 {
	switch value := result[name].(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	default:
		return 0
	}
}

func stateError(state State) error {
	if state == StateDisabled {
		return ErrDisabled
	}
	return ErrUnavailable
}

type observerValue struct{ plugin coreusage.Plugin }

type observerProxy struct {
	target  atomic.Pointer[observerValue]
	onUsage func()
}

func newObserverProxy() *observerProxy { return &observerProxy{} }

func (p *observerProxy) set(plugin coreusage.Plugin) {
	if p == nil || plugin == nil {
		return
	}
	p.target.Store(&observerValue{plugin: plugin})
}

func (p *observerProxy) clear() {
	if p != nil {
		p.target.Store(nil)
	}
}

func (p *observerProxy) TraceCaptureActive() bool {
	return p != nil && p.target.Load() != nil
}

// HandleProxyResponse forwards the downstream outcome to the active adapter.
func (p *observerProxy) HandleProxyResponse(response coreusage.ProxyResponse) {
	if p == nil {
		return
	}
	defer func() { _ = recover() }()
	value := p.target.Load()
	if value == nil {
		return
	}
	if adapter, ok := value.plugin.(*collector.Adapter); ok {
		adapter.HandleProxyResponse(response)
	}
}

func (p *observerProxy) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil {
		return
	}
	defer func() { _ = recover() }()
	value := p.target.Load()
	if value != nil && value.plugin != nil {
		value.plugin.HandleUsage(ctx, record)
		if p.onUsage != nil {
			p.onUsage()
		}
	}
}

type readerProxy struct{ service *service }

type maintenanceProxy struct{ service *service }

func (p *maintenanceProxy) target() Maintenance {
	if p == nil || p.service == nil {
		return unavailableMaintenance{err: ErrUnavailable}
	}
	p.service.mu.RLock()
	defer p.service.mu.RUnlock()
	if p.service.closed {
		return unavailableMaintenance{err: ErrClosed}
	}
	if !p.service.config.Enabled {
		return unavailableMaintenance{err: ErrDisabled}
	}
	if p.service.maint == nil {
		return unavailableMaintenance{err: ErrUnavailable}
	}
	return p.service.maint
}

func (p *maintenanceProxy) Start(ctx context.Context, request MaintenanceRequest) (model.JobStatus, error) {
	if p != nil && p.service != nil {
		if err := p.service.maintenanceReady(ctx); err != nil {
			return model.JobStatus{}, err
		}
	}
	return p.target().Start(ctx, request)
}

// Maintenance requires a live intake owner and reachable storage. Health may be
// degraded by the maintenance failure that this operation is intended to repair.
func (s *service) maintenanceReady(ctx context.Context) error {
	s.mu.RLock()
	backend, current, active, closed, enabled := s.backend, s.collector, s.maintenanceActive, s.closed, s.config.Enabled
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if !enabled {
		return ErrDisabled
	}
	if backend == nil || current == nil {
		return ErrUnavailable
	}
	if active {
		return nil
	}
	if !current.Ready() {
		return ErrUnavailable
	}
	if ready, ok := backend.(interface{ CheckReady(context.Context) error }); ok {
		if err := ready.CheckReady(ctx); err != nil {
			return &UnavailableError{Category: "storage_unavailable"}
		}
		return nil
	}
	if database, ok := backend.(maintenance.Store); ok {
		if err := database.Checkpoint(ctx); err != nil {
			return &UnavailableError{Category: "storage_unavailable"}
		}
		return nil
	}
	return nil
}

func (p *maintenanceProxy) Status(ctx context.Context, jobID string) (model.JobStatus, error) {
	return p.target().Status(ctx, jobID)
}

func (p *maintenanceProxy) Cancel(ctx context.Context, jobID string) error {
	return p.target().Cancel(ctx, jobID)
}

type maintenanceAdapter struct{ controller *maintenance.Controller }

func (a maintenanceAdapter) Start(ctx context.Context, request MaintenanceRequest) (model.JobStatus, error) {
	return a.controller.Start(ctx, maintenance.Request{Kind: request.Kind, Options: request.Options})
}

func (a maintenanceAdapter) Status(ctx context.Context, jobID string) (model.JobStatus, error) {
	return a.controller.Status(ctx, jobID)
}

func (a maintenanceAdapter) Cancel(ctx context.Context, jobID string) error {
	return a.controller.Cancel(ctx, jobID)
}

func (a maintenanceAdapter) Close(ctx context.Context) error {
	return a.controller.Close(ctx)
}

func (r *readerProxy) Summary(ctx context.Context, query model.Query) (result model.Summary, err error) {
	backend, err := r.service.backendForRead()
	if err != nil {
		return result, err
	}
	err = recoverQuery(func() error { result, err = backend.Summary(ctx, query); return err })
	r.recordQueryFailure(err)
	r.decorateMeta(&result.Meta, err)
	return result, err
}

func (r *readerProxy) Timeseries(ctx context.Context, query model.Query) (result model.Timeseries, err error) {
	backend, err := r.service.backendForRead()
	if err != nil {
		return result, err
	}
	err = recoverQuery(func() error { result, err = backend.Timeseries(ctx, query); return err })
	r.recordQueryFailure(err)
	r.decorateMeta(&result.Meta, err)
	return result, err
}

func (r *readerProxy) Dimensions(ctx context.Context, query model.Query) (result model.DimensionPage, err error) {
	backend, err := r.service.backendForRead()
	if err != nil {
		return result, err
	}
	err = recoverQuery(func() error { result, err = backend.Dimensions(ctx, query); return err })
	r.recordQueryFailure(err)
	r.decorateMeta(&result.Meta, err)
	return result, err
}

func (r *readerProxy) Events(ctx context.Context, query model.Query) (result model.EventPage, err error) {
	backend, err := r.service.backendForRead()
	if err != nil {
		return result, err
	}
	err = recoverQuery(func() error { result, err = backend.Events(ctx, query); return err })
	r.recordQueryFailure(err)
	r.decorateMeta(&result.Meta, err)
	return result, err
}

func (r *readerProxy) EventsForExport(ctx context.Context, query model.Query) (result model.EventPage, err error) {
	backend, err := r.service.backendForRead()
	if err != nil {
		return result, err
	}
	exporter, ok := backend.(interface {
		EventsForExport(context.Context, model.Query) (model.EventPage, error)
	})
	if !ok {
		return r.Events(ctx, query)
	}
	err = recoverQuery(func() error { result, err = exporter.EventsForExport(ctx, query); return err })
	r.recordQueryFailure(err)
	r.decorateMeta(&result.Meta, err)
	return result, err
}

func (s *service) EventByAttemptID(ctx context.Context, attemptID string, query model.Query) (result model.Event, found bool, err error) {
	backend, err := s.backendForRead()
	if err != nil {
		return result, false, err
	}
	lookup, ok := backend.(EventLookup)
	if !ok {
		return result, false, ErrUnavailable
	}
	err = recoverQuery(func() error {
		result, found, err = lookup.EventByAttemptID(ctx, attemptID, query)
		return err
	})
	s.reader.recordQueryFailure(err)
	return result, found, err
}

func (r *readerProxy) Leaderboard(ctx context.Context, query model.Query) (result model.LeaderboardPage, err error) {
	backend, err := r.service.backendForRead()
	if err != nil {
		return result, err
	}
	err = recoverQuery(func() error { result, err = backend.Leaderboard(ctx, query); return err })
	r.recordQueryFailure(err)
	r.decorateMeta(&result.Meta, err)
	return result, err
}

func (r *readerProxy) ModelLeaderboard(ctx context.Context, query model.Query) (result model.ModelLeaderboard, err error) {
	backend, err := r.service.backendForRead()
	if err != nil {
		return result, err
	}
	err = recoverQuery(func() error { result, err = backend.ModelLeaderboard(ctx, query); return err })
	r.recordQueryFailure(err)
	return result, err
}

func (r *readerProxy) Activity(ctx context.Context, query model.Query) (result model.Activity, err error) {
	backend, err := r.service.backendForRead()
	if err != nil {
		return result, err
	}
	reader, ok := backend.(ReaderV2)
	if !ok {
		return result, ErrUnavailable
	}
	err = recoverQuery(func() error { result, err = reader.Activity(ctx, query); return err })
	r.recordQueryFailure(err)
	r.decorateMeta(&result.Meta, err)
	return result, err
}

func (r *readerProxy) Analysis(ctx context.Context, query model.Query) (result model.Analysis, err error) {
	backend, err := r.service.backendForRead()
	if err != nil {
		return result, err
	}
	reader, ok := backend.(ReaderV2)
	if !ok {
		return result, ErrUnavailable
	}
	err = recoverQuery(func() error { result, err = reader.Analysis(ctx, query); return err })
	r.recordQueryFailure(err)
	r.decorateMeta(&result.Meta, err)
	return result, err
}

func (r *readerProxy) decorateMeta(meta *model.ResponseMeta, err error) {
	if r == nil || r.service == nil || meta == nil || err != nil {
		return
	}
	health := r.service.Health()
	meta.Degraded = health.State == StateDegraded || health.State == StateCircuitOpen
	meta.DroppedEvents = health.Queue.Dropped
	meta.LastSuccessfulWriteAt = cloneTime(health.LastSuccessfulWrite)
}

func startupErrorCategory(err error) string {
	var classified StoreErrorClassification
	if errors.As(err, &classified) {
		if category := classified.Category(); category != "" {
			return category
		}
	}
	if errors.Is(err, ErrDisabled) {
		return "disabled"
	}
	return "storage_start"
}

func (r *readerProxy) recordQueryFailure(err error) {
	if !errors.Is(err, ErrInternal) {
		return
	}
	r.service.snapshots.mutate(func(health *model.Health) {
		health.State = StateDegraded
		health.Category = "query_panic"
		health.Message = "An analytics query failed."
	})
}

func recoverQuery(call func() error) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrInternal
		}
	}()
	return call()
}

type unavailableMaintenance struct{ err error }

func (u unavailableMaintenance) Start(context.Context, MaintenanceRequest) (model.JobStatus, error) {
	return model.JobStatus{}, u.err
}
func (u unavailableMaintenance) Status(context.Context, string) (model.JobStatus, error) {
	return model.JobStatus{}, u.err
}
func (u unavailableMaintenance) Cancel(context.Context, string) error { return u.err }
