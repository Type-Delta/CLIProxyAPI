package cpauk

import (
	"context"
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/chstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestR2StartupReloadUsesLatestConfig(t *testing.T) {
	cfg := smallConfig()
	entered, release := make(chan struct{}), make(chan struct{})
	received := make(chan Config, 2)
	var calls atomic.Int32
	s := New(context.Background(), cfg, func(ctx context.Context, c Config) (Backend, [32]byte, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return nil, [32]byte{}, errors.New("temporary connection failure")
		}
		received <- c
		return &fakeBackend{}, [32]byte{1}, nil
	}).(*service)
	defer s.Close(context.Background())
	<-entered
	cfg.HotRetentionDays = 7
	cfg.BatchSize = 1
	if result := s.Reconfigure(cfg); !result.Applied || result.Error != nil {
		t.Fatal(result)
	}
	close(release)
	select {
	case got := <-received:
		waitServiceState(t, s, StateReady)
		if got.HotRetentionDays != cfg.HotRetentionDays || got.BatchSize != cfg.BatchSize {
			t.Fatalf("accepted reload lost: factory retention=%d batch=%d; saved retention=%d batch=%d", got.HotRetentionDays, got.BatchSize, cfg.HotRetentionDays, cfg.BatchSize)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("retry did not run")
	}
}

type r2RetentionBackend struct {
	*chstore.Store
	calls atomic.Int32
}

func (b *r2RetentionBackend) ApplyRetentionPolicy(ctx context.Context, raw, hourly time.Time, batch int) (chstore.RetentionResult, error) {
	if b.calls.Add(1) == 1 {
		return chstore.RetentionResult{}, errors.New("temporary retention connection failure")
	}
	return chstore.RetentionResult{}, nil
}
func TestR2MaintenanceCanRecoverAfterRetentionFailure(t *testing.T) {
	cfg := smallConfig()
	cfg.Path = filepath.Join(t.TempDir(), "analytics.db")
	handle := chtest.Handle(t)
	database, err := chstore.Open(context.Background(), handle, chstore.Config{Path: cfg.Path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if err = chstore.RunLegacyImport(context.Background(), database, cfg.Path); err != nil {
		t.Fatal(err)
	}
	backend := &r2RetentionBackend{Store: database}
	s := New(context.Background(), cfg, func(context.Context, Config) (Backend, [32]byte, error) {
		return backend, database.IdentityKeyArray(), nil
	}).(*service)
	defer s.Close(context.Background())
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		s.mu.RLock()
		retention := s.retention
		s.mu.RUnlock()
		if retention != nil {
			if at, err := retention.Status(); at != nil && err != nil {
				break
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("initial retention failure not reached")
		default:
			runtime.Gosched()
		}
	}
	if got := s.Health(); got.State != model.StateDegraded || got.Category != "retention" {
		t.Fatalf("unexpected initial health %+v", got)
	}
	if _, err = handle.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = s.retention.RunOnce(context.Background())
	_, manualErr := s.Maintenance().Start(context.Background(), MaintenanceRequest{Kind: "checkpoint"})
	retryErr := s.Retry(context.Background())
	if err != nil || manualErr != nil || retryErr != nil {
		t.Fatalf("healthy DB cannot recover: scheduled=%v manual=%v retry=%v retention calls=%d", err, manualErr, retryErr, backend.calls.Load())
	}
}

type r2TimedOutCloseBackend struct {
	*fakeBackend
	closing  chan struct{}
	released chan struct{}
}

func (b *r2TimedOutCloseBackend) Close(ctx context.Context) error {
	close(b.closing)
	select {
	case <-b.released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestR2TimedOutTeardownDoesNotCreateSecondOwner(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.ShutdownDrain = 100 * time.Millisecond
	first := &r2TimedOutCloseBackend{fakeBackend: &fakeBackend{}, closing: make(chan struct{}), released: make(chan struct{})}
	var calls atomic.Int32
	secondOpened := make(chan struct{})
	svc := New(ctx, cfg, func(context.Context, Config) (Backend, [32]byte, error) {
		if calls.Add(1) == 1 {
			return first, [32]byte{1}, nil
		}
		close(secondOpened)
		return &fakeBackend{}, [32]byte{1}, nil
	})
	defer svc.Close(ctx)
	waitServiceState(t, svc, StateReady)
	cfg.Enabled = false
	svc.Reconfigure(cfg)
	<-first.closing
	cfg.Enabled = true
	svc.Reconfigure(cfg)
	select {
	case <-secondOpened:
		t.Errorf("second mutation owner opened after timed-out Close while previous worker remains active")
	case <-time.After(500 * time.Millisecond):
	}
	close(first.released)
	waitServiceState(t, svc, StateReady)
}
