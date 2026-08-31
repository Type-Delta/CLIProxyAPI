package cpauk

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/collector"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/maintenance"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type Reader interface {
	Summary(context.Context, model.Query) (model.Summary, error)
	Timeseries(context.Context, model.Query) (model.Timeseries, error)
	Dimensions(context.Context, model.Query) (model.DimensionPage, error)
	Events(context.Context, model.Query) (model.EventPage, error)
	Leaderboard(context.Context, model.Query) (model.LeaderboardPage, error)
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
	Reconfigure(context.Context, Config) error
}

type StorageBudgetReconfigurer interface {
	ReconfigureStorageBudget(context.Context, int64, int64) error
}

type PriceBookBackend interface {
	PriceBook(context.Context) (aggregate.PriceBook, error)
	UpdatePriceBook(context.Context, aggregate.PriceBook) (aggregate.PriceBook, error)
}

type PricingSnapshotBackend interface {
	PricingSnapshot(context.Context) (store.PricingSnapshot, error)
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

	snapshots        *snapshots
	observer         *observerProxy
	reader           *readerProxy
	maint            Maintenance
	maintProxy       *maintenanceProxy
	retention        *maintenance.RetentionScheduler
	identityKey      [32]byte
	hasIdentityKey   bool
	configuredKeyIDs []string
	rotatedKeyIDs    []string
	startSeq         atomic.Uint64
}

// New constructs a failure-isolated service. It never returns an analytics
// startup error to CPA. Enabled storage starts in the background.
func New(ctx context.Context, config Config, factory BackendFactory) Service {
	config = config.WithDefaults()
	if err := config.Validate(); err != nil {
		s := &service{
			factory:     factory,
			config:      config,
			validConfig: false,
			snapshots:   newSnapshots(config, StateCircuitOpen, "invalid_config", "Analytics is unavailable."),
			observer:    newObserverProxy(),
			maint:       unavailableMaintenance{err: &UnavailableError{Category: "invalid_config"}},
		}
		s.reader = &readerProxy{service: s}
		s.maintProxy = &maintenanceProxy{service: s}
		return s
	}
	state := StateDisabled
	if config.Enabled {
		state = StateStarting
	}
	s := &service{
		factory:     factory,
		config:      config,
		validConfig: true,
		snapshots:   newSnapshots(config, state, "", ""),
		observer:    newObserverProxy(),
		maint:       unavailableMaintenance{err: stateError(state)},
	}
	s.reader = &readerProxy{service: s}
	s.maintProxy = &maintenanceProxy{service: s}
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
	s := &service{
		config:      config,
		validConfig: true,
		snapshots:   newSnapshots(config, StateCircuitOpen, category, "Analytics is unavailable."),
		observer:    newObserverProxy(),
		maint:       unavailableMaintenance{err: &UnavailableError{Category: category}},
	}
	s.reader = &readerProxy{service: s}
	s.maintProxy = &maintenanceProxy{service: s}
	return s
}

// NewInvalid preserves the backend factory so a corrected isolated analytics
// configuration can start on hot reload without restarting CPA.
func NewInvalid(category, field string, config Config, factory BackendFactory) Service {
	config = config.WithDefaults()
	if category == "" {
		category = "invalid_config"
	}
	s := &service{
		factory: factory, config: config, validConfig: false,
		snapshots: newSnapshots(config, StateCircuitOpen, category, "Analytics is unavailable."),
		observer:  newObserverProxy(), maint: unavailableMaintenance{err: &UnavailableError{Category: category}},
	}
	s.snapshots.mutate(func(health *model.Health) { health.Field = field })
	s.reader = &readerProxy{service: s}
	s.maintProxy = &maintenanceProxy{service: s}
	return s
}

func (s *service) Observer() coreusage.Plugin { return s.observer }
func (s *service) Reader() Reader             { return s.reader }
func (s *service) Maintenance() Maintenance   { return s.maintProxy }

func (s *service) Capabilities() Capabilities {
	value := s.snapshots.load().capabilities
	s.mu.RLock()
	if s.maintenanceActive {
		value.Available = false
	} else if value.State == StateCircuitOpen {
		value.Available = s.backend != nil && !s.closed && s.config.Enabled && !s.maintenanceActive
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
	if stats, ok := s.collectorStats(); ok {
		value.Queue.Capacity = stats.Capacity
		value.Queue.Depth = stats.Depth
		value.Queue.Dropped = stats.Dropped
		value.RejectedEvents = stats.Rejected
		value.TruncatedFields = stats.TruncatedFields
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
		s.startSeq.Add(1)
		if s.startCancel != nil {
			s.startCancel()
			s.startCancel = nil
		}
		s.starting = false
		s.observer.clear()
		s.mu.Unlock()
		s.snapshots.setConfig(config)
		s.snapshots.mutate(func(health *model.Health) {
			health.State = StateDisabled
			health.Category = ""
			health.Field = ""
			health.Message = ""
		})
		go closeServiceGeneration(oldMaintenance, oldRetention, oldCollector, oldBackend, config.ShutdownDrain)
		return ReconfigureResult{Applied: true}
	}

	if !previous.Enabled && config.Enabled {
		s.config = config
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
		s.config = config
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
	s.mu.Lock()
	s.config = config
	s.mu.Unlock()
	s.snapshots.setConfig(config)
	return ReconfigureResult{Applied: true}
}

func (s *service) Retry(_ context.Context) error {
	s.mu.RLock()
	currentCollector := s.collector
	config := s.config
	closed := s.closed
	s.mu.RUnlock()
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
	if !currentCollector.Retry() {
		return ErrUnavailable
	}
	return nil
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
	return pricing.UpdatePriceBook(ctx, book)
}

func (s *service) PricingSnapshot(ctx context.Context) (snapshot store.PricingSnapshot, err error) {
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
			snapshot = store.PricingSnapshot{}
			err = ErrInternal
		}
	}()
	return pricing.PricingSnapshot(ctx)
}

// MarkRestartRequired lets the CPA integration surface restart-only settings
// that intentionally remain outside the CPAUK configuration boundary.
func (s *service) MarkRestartRequired(_ []string) {
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
	s.mu.RUnlock()
	if !hasIdentityKey {
		return nil, ErrUnavailable
	}
	return model.CredentialID(identityKey[:], provider, authIndex, authID)
}

func (s *service) ReplaceProviderQuotaSnapshots(ctx context.Context, snapshots []store.ProviderQuotaSnapshot) (err error) {
	backend, err := s.backendForRead()
	if err != nil {
		return err
	}
	provider, ok := backend.(store.ProviderQuotaStore)
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

func (s *service) ProviderQuotaSnapshots(ctx context.Context) (snapshots []store.ProviderQuotaSnapshot, err error) {
	backend, err := s.backendForRead()
	if err != nil {
		return nil, err
	}
	provider, ok := backend.(store.ProviderQuotaStore)
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

func (s *service) KeyCatalog(ctx context.Context, query model.Query) (page store.KeyCatalogPage, err error) {
	backend, err := s.backendForRead()
	if err != nil {
		return page, err
	}
	provider, ok := backend.(interface {
		KeyCatalog(context.Context, model.Query) (store.KeyCatalogPage, error)
	})
	if !ok {
		return page, ErrUnavailable
	}
	defer func() {
		if recover() != nil {
			page = store.KeyCatalogPage{}
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
	return provider.UpdateKeyLifecycle(ctx, configuredIDs, rotatedIDs)
}

func (s *service) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.startSeq.Add(1)
	if s.startCancel != nil {
		s.startCancel()
		s.startCancel = nil
	}
	currentCollector, backend, currentMaintenance, currentRetention := s.collector, s.backend, s.maint, s.retention
	s.collector, s.backend, s.sanitizer = nil, nil, nil
	s.identityKey, s.hasIdentityKey = [32]byte{}, false
	s.maintenanceActive = false
	s.maint = unavailableMaintenance{err: ErrClosed}
	s.retention = nil
	drain := s.config.ShutdownDrain
	if drain <= 0 || drain > time.Minute {
		drain = DefaultShutdownDrain
	}
	s.observer.clear()
	s.mu.Unlock()
	s.snapshots.mutate(func(health *model.Health) { health.State = StateStopping })

	if ctx == nil {
		ctx = context.Background()
	}
	drainCtx, cancel := context.WithTimeout(ctx, drain)
	defer cancel()
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
	startDone := make(chan struct{})
	go func() {
		s.startWG.Wait()
		close(startDone)
	}()
	select {
	case <-startDone:
	case <-drainCtx.Done():
		errs = append(errs, fmt.Errorf("wait for analytics startup: %w", drainCtx.Err()))
	}
	return errors.Join(errs...)
}

func (s *service) beginStart(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.closed || s.starting || !s.config.Enabled {
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
		s.start(startCtx, sequence, config)
	}()
}

func (s *service) start(ctx context.Context, sequence uint64, config Config) {
	backend, identityKey, err := callFactory(ctx, s.factory, config)
	if err != nil {
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
		_ = safeBackendClose(context.Background(), backend)
		s.finishStartFailure(sequence, "collector_start")
		return
	}

	s.mu.Lock()
	if s.closed || sequence != s.startSeq.Load() || !s.config.Enabled {
		s.mu.Unlock()
		closeGeneration(currentCollector, backend, config.ShutdownDrain)
		return
	}
	if lifecycle, ok := backend.(interface {
		UpdateKeyLifecycle(context.Context, []string, []string) error
	}); ok {
		configuredKeyIDs := append([]string(nil), s.configuredKeyIDs...)
		rotatedKeyIDs := append([]string(nil), s.rotatedKeyIDs...)
		if err = safeKeyLifecycleUpdate(ctx, lifecycle, configuredKeyIDs, rotatedKeyIDs); err != nil {
			s.mu.Unlock()
			closeGeneration(currentCollector, backend, config.ShutdownDrain)
			s.finishStartFailure(sequence, "key_lifecycle")
			return
		}
	}
	s.backend = backend
	s.collector = currentCollector
	s.sanitizer = sanitizer
	s.identityKey = identityKey
	s.hasIdentityKey = true
	var retention *maintenance.RetentionScheduler
	if database, ok := backend.(maintenance.Store); ok {
		var stateBeforeMaintenance model.Health
		controller := maintenance.New(maintenance.Hooks{
			Detach: func(context.Context) error {
				s.mu.Lock()
				s.maintenanceActive = true
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
			Drain: currentCollector.Drain,
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
			closeGeneration(currentCollector, backend, config.ShutdownDrain)
			s.finishStartFailure(sequence, "retention_start")
			return
		}
		if err = retention.SetRunner(func(runContext context.Context, rawCutoff, hourlyCutoff time.Time, _ int) (store.RetentionResult, error) {
			result, runErr := runScheduledRetention(runContext, controller, rawCutoff, hourlyCutoff)
			if errors.Is(runErr, maintenance.ErrJobRunning) {
				return store.RetentionResult{}, nil
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
				health.RetentionCutoff = cloneTime(&rawCutoff)
			})
			return result, nil
		}); err != nil {
			s.mu.Unlock()
			_ = controller.Close(context.Background())
			closeGeneration(currentCollector, backend, config.ShutdownDrain)
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

func closeGeneration(currentCollector *collector.Collector, backend Backend, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if currentCollector != nil {
		_ = currentCollector.Close(ctx)
	}
	if backend != nil {
		_ = safeBackendClose(ctx, backend)
	}
}

func closeServiceGeneration(currentMaintenance Maintenance, retention *maintenance.RetentionScheduler, currentCollector *collector.Collector, backend Backend, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if retention != nil {
		_ = retention.Close(ctx)
	}
	if closer, ok := currentMaintenance.(interface{ Close(context.Context) error }); ok {
		_ = closer.Close(ctx)
	}
	if currentCollector != nil {
		_ = currentCollector.Close(ctx)
	}
	if backend != nil {
		_ = safeBackendClose(ctx, backend)
	}
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
	return backend.Reconfigure(context.Background(), config)
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

func runScheduledRetention(ctx context.Context, controller *maintenance.Controller, rawCutoff, hourlyCutoff time.Time) (store.RetentionResult, error) {
	job, err := controller.Start(ctx, maintenance.Request{Kind: "retention", Options: map[string]any{
		"raw_cutoff": rawCutoff.Format(time.RFC3339Nano), "hourly_cutoff": hourlyCutoff.Format(time.RFC3339Nano),
	}})
	if err != nil {
		return store.RetentionResult{}, err
	}
	status, err := controller.Await(ctx, job.JobID)
	if err != nil {
		return store.RetentionResult{}, err
	}
	if status.State != model.JobSucceeded {
		return store.RetentionResult{}, fmt.Errorf("scheduled analytics retention failed")
	}
	return store.RetentionResult{
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

type observerProxy struct{ target atomic.Pointer[observerValue] }

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

func (p *observerProxy) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil {
		return
	}
	defer func() { _ = recover() }()
	value := p.target.Load()
	if value != nil && value.plugin != nil {
		value.plugin.HandleUsage(ctx, record)
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
	return p.target().Start(ctx, request)
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
