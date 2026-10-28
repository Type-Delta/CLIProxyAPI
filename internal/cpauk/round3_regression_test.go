package cpauk

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type r3CloseErrorBackend struct {
	*fakeBackend
	closeCalled chan struct{}
	released    atomic.Bool
}

func (b *r3CloseErrorBackend) Close(context.Context) error {
	select {
	case <-b.closeCalled:
	default:
		close(b.closeCalled)
	}
	if !b.released.Load() {
		return errors.New("temporary close failure, worker still owns storage")
	}
	return nil
}
func TestR3SupersededStartupCloseErrorRetainsOwnership(t *testing.T) {
	entered, releaseFactory, secondOpened := make(chan struct{}), make(chan struct{}), make(chan struct{})
	old := &r3CloseErrorBackend{fakeBackend: &fakeBackend{}, closeCalled: make(chan struct{})}
	var calls atomic.Int32
	cfg := smallConfig()
	svc := New(context.Background(), cfg, func(context.Context, Config) (Backend, [32]byte, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-releaseFactory
			return old, [32]byte{1}, nil
		}
		close(secondOpened)
		return &fakeBackend{}, [32]byte{1}, nil
	})
	defer svc.Close(context.Background())
	defer old.released.Store(true)
	<-entered
	cfg.HotRetentionDays = 7
	if got := svc.Reconfigure(cfg); got.Error != nil || !got.Applied {
		t.Fatal(got)
	}
	close(releaseFactory)
	<-old.closeCalled
	select {
	case <-secondOpened:
		t.Fatal("successor factory opened after failed close while superseded backend still owns storage")
	case <-time.After(150 * time.Millisecond):
	}
	old.released.Store(true)
	waitServiceState(t, svc, StateReady)
	if calls.Load() != 2 {
		t.Fatalf("cleanup did not release the replacement startup: factories=%d", calls.Load())
	}
}

func TestR3FailedDisableTeardownRetriesAndReportsHealth(t *testing.T) {
	cfg := smallConfig()
	old := &r3CloseErrorBackend{fakeBackend: &fakeBackend{}, closeCalled: make(chan struct{})}
	var factories atomic.Int32
	svc := New(context.Background(), cfg, func(context.Context, Config) (Backend, [32]byte, error) {
		if factories.Add(1) == 1 {
			return old, [32]byte{1}, nil
		}
		return &fakeBackend{}, [32]byte{2}, nil
	})
	defer svc.Close(context.Background())
	defer old.released.Store(true)
	waitServiceState(t, svc, StateReady)
	cfg.Enabled = false
	svc.Reconfigure(cfg)
	<-old.closeCalled
	cfg.Enabled = true
	svc.Reconfigure(cfg)
	if err := svc.Retry(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForService(t, func() bool { return svc.Health().Category != "" })
	health := svc.Health()
	if health.Message == "" {
		t.Fatalf("failed teardown is invisible: %+v", health)
	}
	if factories.Load() != 1 {
		t.Fatal("failed teardown released ownership")
	}
	// Another disable must not chain onto a dead, abandoned barrier.
	cfg.Enabled = false
	svc.Reconfigure(cfg)
	cfg.Enabled = true
	svc.Reconfigure(cfg)
	old.released.Store(true)
	waitServiceState(t, svc, StateReady)
	if factories.Load() != 2 {
		t.Fatalf("successful cleanup did not release successor: factories=%d", factories.Load())
	}
}

type r3FailOnceCloseBackend struct {
	*fakeBackend
	firstFailed chan struct{}
	attempts    atomic.Int32
}

func (b *r3FailOnceCloseBackend) Close(context.Context) error {
	if b.attempts.Add(1) == 1 {
		close(b.firstFailed)
		return errors.New("transient close failure")
	}
	return nil
}
func TestR3TransientCloseErrorRecoversWithoutRestart(t *testing.T) {
	cfg := smallConfig()
	old := &r3FailOnceCloseBackend{fakeBackend: &fakeBackend{}, firstFailed: make(chan struct{})}
	var factories atomic.Int32
	svc := New(context.Background(), cfg, func(context.Context, Config) (Backend, [32]byte, error) {
		if factories.Add(1) == 1 {
			return old, [32]byte{1}, nil
		}
		return &fakeBackend{}, [32]byte{2}, nil
	})
	defer svc.Close(context.Background())
	waitServiceState(t, svc, StateReady)
	cfg.Enabled = false
	svc.Reconfigure(cfg)
	<-old.firstFailed
	cfg.Enabled = true
	svc.Reconfigure(cfg)
	waitServiceState(t, svc, StateReady)
	if old.attempts.Load() != 2 || factories.Load() != 2 {
		t.Fatalf("cleanup attempts=%d factories=%d", old.attempts.Load(), factories.Load())
	}
	if svc.Health().Category == "teardown_failure" {
		t.Fatal("completed cleanup retained a stale failure")
	}
}
