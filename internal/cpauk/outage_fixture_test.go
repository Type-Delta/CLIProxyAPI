package cpauk

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/chstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// waitOutageFixtureReady is the setup readiness predicate used by the outage test.
func waitOutageFixtureReady(ctx context.Context, s *service) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.RLock()
		current, retention, active := s.collector, s.retention, s.maintenanceActive
		s.mu.RUnlock()
		completed := retention == nil
		if retention != nil {
			at, err := retention.Status()
			if at != nil && err != nil {
				return fmt.Errorf("startup retention failed: %w", err)
			}
			completed = at != nil
		}
		if completed && !active && current.Ready() && s.Health().State == StateReady {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type r3StartupRetentionBackend struct {
	*chstore.Store
	writeCommitted, returnWrite, startRetention, retentionEntered, finishRetention chan struct{}
	once                                                                           sync.Once
}

func (b *r3StartupRetentionBackend) WaitLegacyImport(ctx context.Context) error {
	select {
	case <-b.startRetention:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (b *r3StartupRetentionBackend) WriteBatchWithPatches(ctx context.Context, events []model.Event, patches []model.ProxyResponsePatch) error {
	if err := b.Store.WriteBatchWithPatches(ctx, events, patches); err != nil {
		return err
	}
	b.once.Do(func() {
		close(b.writeCommitted)
		select {
		case <-b.returnWrite:
		case <-ctx.Done():
		}
	})
	return ctx.Err()
}
func (b *r3StartupRetentionBackend) ApplyRetentionPolicy(ctx context.Context, raw, hourly time.Time, batch int) (chstore.RetentionResult, error) {
	close(b.retentionEntered)
	select {
	case <-b.finishRetention:
		return b.Store.ApplyRetentionPolicy(ctx, raw, hourly, batch)
	case <-ctx.Done():
		return chstore.RetentionResult{}, ctx.Err()
	}
}

func TestR3OutageSetupWaitsForStartupMaintenance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := smallConfig()
	cfg.Path = filepath.Join(t.TempDir(), "analytics.db")
	h := chtest.Handle(t)
	st, err := chstore.Open(ctx, h, chstore.Config{Path: cfg.Path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if err = chstore.RunLegacyImport(ctx, st, cfg.Path); err != nil {
		t.Fatal(err)
	}
	b := &r3StartupRetentionBackend{Store: st, writeCommitted: make(chan struct{}), returnWrite: make(chan struct{}), startRetention: make(chan struct{}), retentionEntered: make(chan struct{}), finishRetention: make(chan struct{})}
	svc := New(context.Background(), cfg, func(context.Context, Config) (Backend, [32]byte, error) { return b, st.IdentityKeyArray(), nil }).(*service)
	defer svc.Close(context.Background())
	var returnOnce, finishOnce sync.Once
	returnWrite := func() { returnOnce.Do(func() { close(b.returnWrite) }) }
	finishRetention := func() { finishOnce.Do(func() { close(b.finishRetention) }) }
	defer func() { returnWrite(); finishRetention() }()
	waitServiceState(t, svc, StateReady)
	svc.Observer().HandleUsage(context.Background(), validServiceRecord())
	select {
	case <-b.writeCommitted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(b.startRetention)
	waitForService(t, func() bool { svc.mu.RLock(); defer svc.mu.RUnlock(); return svc.maintenanceActive })
	returnWrite()
	select {
	case <-b.retentionEntered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// This is the real collector callback, not a synthetic health mutation.
	if svc.Health().State != StateReady {
		t.Fatalf("did not reproduce collector-ready pulse: %+v", svc.Health())
	}
	done := make(chan error, 1)
	go func() { done <- waitOutageFixtureReady(ctx, svc) }()
	select {
	case err := <-done:
		t.Fatalf("outage setup accepted a Ready pulse while startup retention was active: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	finishRetention()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
