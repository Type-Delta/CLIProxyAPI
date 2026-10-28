package cpauk

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type r4OwnedBackend struct {
	*fakeBackend
	owners  *atomic.Int32
	done    atomic.Bool
	release *atomic.Bool
}

func (b *r4OwnedBackend) Close(context.Context) error {
	if b.release != nil && !b.release.Load() {
		return errors.New("retain old owner")
	}
	if b.done.CompareAndSwap(false, true) {
		b.owners.Add(-1)
	}
	return nil
}
func TestR4RetryReloadNoDoubleStartStress(t *testing.T) {
	for round := 0; round < 200; round++ {
		var owners, opens, peak atomic.Int32
		var release atomic.Bool
		var lock sync.Mutex
		var backends []*r4OwnedBackend
		cfg := smallConfig()
		s := New(context.Background(), cfg, func(context.Context, Config) (Backend, [32]byte, error) {
			n := opens.Add(1)
			v := owners.Add(1)
			for {
				p := peak.Load()
				if v <= p || peak.CompareAndSwap(p, v) {
					break
				}
			}
			b := &r4OwnedBackend{fakeBackend: &fakeBackend{}, owners: &owners}
			if n == 1 {
				b.release = &release
			}
			lock.Lock()
			backends = append(backends, b)
			lock.Unlock()
			return b, [32]byte{}, nil
		}).(*service)
		waitServiceState(t, s, StateReady)
		cfg.Enabled = false
		s.Reconfigure(cfg)
		waitForService(t, func() bool { return s.Health().Category == "teardown_failure" })
		cfg.Enabled = true
		s.Reconfigure(cfg)
		gate := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				for j := 0; j < 80; j++ {
					_ = s.Retry(context.Background())
					runtime.Gosched()
				}
			}()
		}
		release.Store(true)
		close(gate)
		wg.Wait()
		waitServiceState(t, s, StateReady)
		_ = s.Close(context.Background())
		lock.Lock()
		for _, b := range backends {
			_ = b.Close(context.Background())
		}
		lock.Unlock()
		if peak.Load() > 1 || opens.Load() != 2 {
			t.Fatalf("round %d: overlapping owners=%d factories=%d", round, peak.Load(), opens.Load())
		}
	}
}

type r4TerminalBackend struct {
	*fakeBackend
	release      chan struct{}
	called       chan struct{}
	exited       chan struct{}
	once         sync.Once
	exitOnce     sync.Once
	released     atomic.Bool
	attempts     atomic.Int32
	active       atomic.Int32
	block        bool
	ignoreCancel bool
}

func (b *r4TerminalBackend) Close(ctx context.Context) error {
	b.attempts.Add(1)
	b.active.Add(1)
	defer b.active.Add(-1)
	b.once.Do(func() { close(b.called) })
	if b.block {
		defer b.exitOnce.Do(func() { close(b.exited) })
		if b.ignoreCancel {
			<-b.release
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-b.release:
			return nil
		}
	}
	if !b.released.Load() {
		return errors.New("permanent cleanup failure; storage ownership unresolved")
	}
	return nil
}
func r4FinalizerCount() int {
	buf := make([]byte, 2<<20)
	n := runtime.Stack(buf, true)
	count := 0
	for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.Contains(stack, "(*service).retireGenerationLocked.func1()") {
			count++
		}
	}
	return count
}
func r4FailingService(t *testing.T, b *r4TerminalBackend) (*service, Config) {
	t.Helper()
	cfg := smallConfig()
	s := New(context.Background(), cfg, func(context.Context, Config) (Backend, [32]byte, error) { return b, [32]byte{}, nil }).(*service)
	waitServiceState(t, s, StateReady)
	cfg.Enabled = false
	s.Reconfigure(cfg)
	<-b.called
	return s, cfg
}
func r4ReleaseFixture(t *testing.T, s *service, b *r4TerminalBackend, baseline int) {
	t.Helper()
	b.released.Store(true)
	if b.release != nil {
		close(b.release)
	}
	_ = s.Retry(context.Background())
	waitForService(t, func() bool { return r4FinalizerCount() == baseline })
}
func TestR4TerminalCloseStopsFailingFinalizers(t *testing.T) {
	baseline := r4FinalizerCount()
	b := &r4TerminalBackend{fakeBackend: &fakeBackend{}, called: make(chan struct{})}
	s, cfg := r4FailingService(t, b)
	defer r4ReleaseFixture(t, s, b, baseline)
	waitForService(t, func() bool { return s.Health().Category == "teardown_failure" })
	for range 12 {
		cfg.Enabled = true
		s.Reconfigure(cfg)
		cfg.Enabled = false
		s.Reconfigure(cfg)
	}
	cfg.Enabled = true
	s.Reconfigure(cfg)
	if err := s.Close(context.Background()); err == nil {
		t.Error("terminal Close reported successful ownership release")
	}
	if err := s.Close(context.Background()); err == nil {
		t.Error("second Close falsely reported resolved cleanup")
	}
	s.mu.RLock()
	barrier := s.teardownDone
	s.mu.RUnlock()
	select {
	case <-barrier:
		t.Error("cancellation released unresolved ownership")
	default:
	}
	deadline := time.Now().Add(150 * time.Millisecond)
	for r4FinalizerCount() != baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if got := r4FinalizerCount() - baseline; got != 0 {
		t.Errorf("terminal Close retained %d finalizer goroutines", got)
	}
	s.mu.RLock()
	pending := s.teardownPending
	s.mu.RUnlock()
	if pending != 0 {
		t.Errorf("terminal Close did not join %d finalizers", pending)
	}

	attempts := b.attempts.Load()
	timer := time.NewTimer(150 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	if got := b.attempts.Load(); got != attempts {
		t.Errorf("cleanup kept retrying after terminal Close: %d -> %d", attempts, got)
	}
}
func TestR4TerminalCloseCancelsBackendClose(t *testing.T) {
	baseline := r4FinalizerCount()
	b := &r4TerminalBackend{fakeBackend: &fakeBackend{}, called: make(chan struct{}), exited: make(chan struct{}), release: make(chan struct{}), block: true}
	s, _ := r4FailingService(t, b)
	defer r4ReleaseFixture(t, s, b, baseline)
	if err := s.Close(context.Background()); err == nil {
		t.Error("canceled backend close reported successful cleanup")
	}
	if err := s.Close(context.Background()); err == nil {
		t.Error("second Close omitted unresolved ownership")
	}
	select {
	case <-b.exited:
	case <-time.After(150 * time.Millisecond):
		t.Error("backend Close never received terminal cancellation")
	}
	if got := b.active.Load(); got != 0 {
		t.Errorf("backend Close is still active: %d", got)
	}
}
func TestR4SecondCloseJoinsOrReportsBlockedFinalizer(t *testing.T) {
	baseline := r4FinalizerCount()
	b := &r4TerminalBackend{fakeBackend: &fakeBackend{}, called: make(chan struct{}), exited: make(chan struct{}), release: make(chan struct{}), block: true, ignoreCancel: true}
	s, _ := r4FailingService(t, b)
	var released sync.Once
	release := func() { released.Do(func() { close(b.release) }) }
	defer func() { release(); waitForService(t, func() bool { return r4FinalizerCount() == baseline }) }()
	if err := s.Close(context.Background()); err == nil {
		t.Error("blocked finalizer was not reported")
	}
	if err := s.Close(context.Background()); err == nil {
		t.Error("second Close falsely succeeded while finalizer still ran")
	}
	release()
	waitForService(t, func() bool { return r4FinalizerCount() == baseline })
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("completed finalization did not join cleanly: %v", err)
	}
}

type r4LateBackend struct {
	*fakeBackend
	stopped chan struct{}
	once    sync.Once
}

func (b *r4LateBackend) StopBackground()                 { b.once.Do(func() { close(b.stopped) }) }
func (b *r4LateBackend) Close(ctx context.Context) error { return ctx.Err() }
func TestR4LateStartupOwnerGetsTerminalCancellation(t *testing.T) {
	baseline := r4FinalizerCount()
	b := &r4LateBackend{fakeBackend: &fakeBackend{}, stopped: make(chan struct{})}
	entered, release := make(chan struct{}), make(chan struct{})
	s := New(context.Background(), smallConfig(), func(context.Context, Config) (Backend, [32]byte, error) {
		close(entered)
		<-release
		return b, [32]byte{}, nil
	}).(*service)
	<-entered
	defer b.StopBackground()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Close(ctx); err == nil {
		t.Error("unfinished startup reported successful shutdown")
	}
	close(release)
	if err := s.Close(context.Background()); err == nil {
		t.Error("late owner cancellation reported released storage")
	}
	select {
	case <-b.stopped:
	case <-time.After(150 * time.Millisecond):
		t.Error("late startup's background worker never received cancellation")
	}
	waitForService(t, func() bool { return r4FinalizerCount() == baseline })
}
