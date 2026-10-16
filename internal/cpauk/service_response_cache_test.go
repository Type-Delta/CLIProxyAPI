package cpauk

import (
	"context"
	"errors"
	"testing"
)

func TestResponseCacheEpochAllowsIntakeButInvalidatesOnReconfigure(t *testing.T) {
	backend := &fakeBackend{}
	s := New(context.Background(), smallConfig(), func(context.Context, Config) (Backend, [32]byte, error) {
		return backend, [32]byte{1}, nil
	}).(*service)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	waitServiceState(t, s, StateReady)
	before, err := s.AnalyticsResponseCacheEpoch()
	if err != nil {
		t.Fatal(err)
	}
	s.Observer().HandleUsage(context.Background(), validServiceRecord())
	waitForService(t, func() bool { return backend.writes.Load() == 1 })
	after, err := s.AnalyticsResponseCacheEpoch()
	if err != nil || after != before {
		t.Fatalf("intake invalidated the shared snapshot: before=%q after=%q err=%v", before, after, err)
	}
	config := smallConfig()
	config.BatchSize++
	if result := s.Reconfigure(config); !result.Applied || result.Error != nil {
		t.Fatalf("reconfigure: %+v", result)
	}
	after, err = s.AnalyticsResponseCacheEpoch()
	if err != nil || after == before {
		t.Fatalf("reconfigure kept the old snapshot: before=%q after=%q err=%v", before, after, err)
	}
	config.Enabled = false
	if result := s.Reconfigure(config); !result.Applied || result.Error != nil {
		t.Fatalf("disable: %+v", result)
	}
	if _, err := s.AnalyticsResponseCacheEpoch(); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled cache access: %v", err)
	}
}

func TestResponseCacheEpochUsesReadAvailability(t *testing.T) {
	s := &service{config: Config{Enabled: true}, backend: &fakeBackend{}}
	before, err := s.AnalyticsResponseCacheEpoch()
	if err != nil {
		t.Fatal(err)
	}
	s.maintenanceActive = true
	s.responseCacheEpoch++
	if _, err := s.AnalyticsResponseCacheEpoch(); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("maintenance cache access: %v", err)
	}
	s.maintenanceActive = false
	after, err := s.AnalyticsResponseCacheEpoch()
	if err != nil || after == before {
		t.Fatalf("maintenance completion kept old snapshot: before=%q after=%q err=%v", before, after, err)
	}
	s.closed = true
	if _, err := s.AnalyticsResponseCacheEpoch(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed cache access: %v", err)
	}
	s.closed = false
	s.backend = nil
	if _, err := s.AnalyticsResponseCacheEpoch(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable cache access: %v", err)
	}
}
