package maintenance

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
)

const DefaultHourlyRetentionDays = 400

type retentionStore interface {
	ApplyRetentionPolicy(context.Context, time.Time, time.Time, int) (store.RetentionResult, error)
}

type RetentionRunner func(context.Context, time.Time, time.Time, int) (store.RetentionResult, error)

type RetentionScheduler struct {
	mu         sync.RWMutex
	database   retentionStore
	runner     RetentionRunner
	hotDays    int
	hourlyDays int
	batchSize  int
	interval   time.Duration
	now        func() time.Time
	wake       chan struct{}
	cancel     context.CancelFunc
	done       chan struct{}
	lastRun    *time.Time
	lastError  error
}

// SetRunner routes scheduled retention through a lifecycle-aware coordinator.
// It must be called before Start.
func (s *RetentionScheduler) SetRunner(runner RetentionRunner) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return fmt.Errorf("retention scheduler is already running")
	}
	if runner == nil {
		return fmt.Errorf("retention runner is required")
	}
	s.runner = runner
	return nil
}

func NewRetentionScheduler(database retentionStore, hotDays int) (*RetentionScheduler, error) {
	if database == nil {
		return nil, fmt.Errorf("retention store is required")
	}
	scheduler := &RetentionScheduler{database: database, batchSize: 1000, interval: 24 * time.Hour,
		now: time.Now, wake: make(chan struct{}, 1)}
	if err := scheduler.setPolicy(hotDays, DefaultHourlyRetentionDays); err != nil {
		return nil, err
	}
	return scheduler, nil
}

func (s *RetentionScheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return fmt.Errorf("retention scheduler is already running")
	}
	runContext, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.run(runContext, s.done)
	return nil
}

func (s *RetentionScheduler) Reconfigure(hotDays int) error {
	s.mu.Lock()
	if err := s.setPolicy(hotDays, DefaultHourlyRetentionDays); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

func (s *RetentionScheduler) RunOnce(ctx context.Context) (store.RetentionResult, error) {
	s.mu.RLock()
	hotDays, hourlyDays, batchSize, now, runner := s.hotDays, s.hourlyDays, s.batchSize, s.now, s.runner
	s.mu.RUnlock()
	midnight := now().UTC().Truncate(24 * time.Hour)
	rawCutoff, hourlyCutoff := midnight.AddDate(0, 0, -hotDays), midnight.AddDate(0, 0, -hourlyDays)
	var result store.RetentionResult
	var err error
	if runner != nil {
		result, err = runner(ctx, rawCutoff, hourlyCutoff, batchSize)
	} else {
		result, err = s.database.ApplyRetentionPolicy(ctx, rawCutoff, hourlyCutoff, batchSize)
	}
	runAt := now().UTC()
	s.mu.Lock()
	s.lastRun, s.lastError = &runAt, err
	s.mu.Unlock()
	return result, err
}

func (s *RetentionScheduler) Status() (*time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var lastRun *time.Time
	if s.lastRun != nil {
		value := *s.lastRun
		lastRun = &value
	}
	return lastRun, s.lastError
}

func (s *RetentionScheduler) Close(ctx context.Context) error {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *RetentionScheduler) run(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		_, _ = s.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

func (s *RetentionScheduler) setPolicy(hotDays, hourlyDays int) error {
	if hotDays < 1 || hourlyDays < hotDays {
		return fmt.Errorf("retention days must be positive and hourly retention must cover raw retention")
	}
	s.hotDays, s.hourlyDays = hotDays, hourlyDays
	return nil
}
