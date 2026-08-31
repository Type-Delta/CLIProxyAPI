package maintenance

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
)

type retentionCall struct {
	raw, hourly time.Time
}

type recordingRetentionStore struct {
	mu    sync.Mutex
	calls []retentionCall
	ready chan struct{}
}

func (s *recordingRetentionStore) ApplyRetentionPolicy(_ context.Context, raw, hourly time.Time, _ int) (store.RetentionResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, retentionCall{raw: raw, hourly: hourly})
	s.mu.Unlock()
	select {
	case s.ready <- struct{}{}:
	default:
	}
	return store.RetentionResult{}, nil
}

func TestRetentionSchedulerRunsDefaultPolicyAndReconfigures(t *testing.T) {
	database := &recordingRetentionStore{ready: make(chan struct{}, 2)}
	scheduler, err := NewRetentionScheduler(database, 90)
	if err != nil {
		t.Fatal(err)
	}
	scheduler.now = func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) }
	if err := scheduler.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-database.ready:
	case <-time.After(time.Second):
		t.Fatal("initial retention did not run")
	}
	if err := scheduler.Reconfigure(30); err != nil {
		t.Fatal(err)
	}
	select {
	case <-database.ready:
	case <-time.After(time.Second):
		t.Fatal("reconfigured retention did not run")
	}
	if err := scheduler.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	database.mu.Lock()
	defer database.mu.Unlock()
	if len(database.calls) < 2 {
		t.Fatalf("retention calls=%d", len(database.calls))
	}
	midnight := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	if database.calls[0].raw != midnight.AddDate(0, 0, -90) || database.calls[0].hourly != midnight.AddDate(0, 0, -400) {
		t.Fatalf("default cutoffs=%+v", database.calls[0])
	}
	if database.calls[1].raw != midnight.AddDate(0, 0, -30) || database.calls[1].hourly != midnight.AddDate(0, 0, -400) {
		t.Fatalf("reconfigured cutoffs=%+v", database.calls[1])
	}
}
