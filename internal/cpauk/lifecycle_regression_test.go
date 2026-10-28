package cpauk

import (
	"context"
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"sync/atomic"
	"testing"
	"time"
)

func TestStartupRecoversWithoutReconfigure(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	var attempts atomic.Int32
	s := New(context.Background(), cfg, func(context.Context, Config) (Backend, [32]byte, error) {
		if attempts.Add(1) == 1 {
			return nil, [32]byte{}, errors.New("temporary outage")
		}
		return &fakeBackend{}, [32]byte{1}, nil
	})
	defer s.Close(context.Background())
	waitServiceState(t, s, StateReady)
	if attempts.Load() < 2 {
		t.Fatal("startup was not retried")
	}
}
func TestMaintenanceRejectsDegradedBeforeDetach(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	s := New(context.Background(), cfg, func(context.Context, Config) (Backend, [32]byte, error) { return &fakeBackend{}, [32]byte{1}, nil }).(*service)
	defer s.Close(context.Background())
	waitServiceState(t, s, StateReady)
	s.snapshots.mutate(func(h *model.Health) { h.State = StateDegraded; h.Category = "storage_write" })
	if _, err := s.Maintenance().Start(context.Background(), MaintenanceRequest{Kind: "backup"}); err == nil {
		t.Fatal("degraded maintenance accepted")
	}
	if s.maintenanceActive {
		t.Fatal("maintenance detached degraded collector")
	}
}

type teardownBackend struct {
	*fakeBackend
	closing, release chan struct{}
}

func (b *teardownBackend) Close(ctx context.Context) error { close(b.closing); <-b.release; return nil }
func TestEnableWaitsForPreviousTeardown(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	first := &teardownBackend{fakeBackend: &fakeBackend{}, closing: make(chan struct{}), release: make(chan struct{})}
	var calls atomic.Int32
	s := New(context.Background(), cfg, func(context.Context, Config) (Backend, [32]byte, error) {
		if calls.Add(1) == 1 {
			return first, [32]byte{1}, nil
		}
		return &fakeBackend{}, [32]byte{1}, nil
	})
	defer s.Close(context.Background())
	waitServiceState(t, s, StateReady)
	cfg.Enabled = false
	s.Reconfigure(cfg)
	<-first.closing
	cfg.Enabled = true
	s.Reconfigure(cfg)
	select {
	case <-time.After(100 * time.Millisecond):
	}
	got := calls.Load()
	close(first.release)
	if got != 1 {
		t.Fatalf("second owner started before teardown: %d", got)
	}
	waitServiceState(t, s, StateReady)
}
func TestRestartRequiredSurvivesReconfigure(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	s := New(context.Background(), cfg, func(context.Context, Config) (Backend, [32]byte, error) { return &fakeBackend{}, [32]byte{}, nil })
	defer s.Close(context.Background())
	waitServiceState(t, s, StateReady)
	s.(*service).MarkRestartRequired([]string{"clickhouse.dsn"})
	cfg.Enabled = false
	s.Reconfigure(cfg)
	if s.Health().Category != "restart_required" {
		t.Fatalf("lost restart hint: %+v", s.Health())
	}
}
