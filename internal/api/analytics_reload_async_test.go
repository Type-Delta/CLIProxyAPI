package api

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk"
)

// blockingAnalyticsService stalls Reconfigure the way a slow ClickHouse
// mutation stalls the store lock, and records the order of analytics calls.
type blockingAnalyticsService struct {
	viewerRouteService
	release chan struct{}
	mu      sync.Mutex
	calls   []string
}

func (s *blockingAnalyticsService) record(call string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
}

func (s *blockingAnalyticsService) Reconfigure(cpauk.Config) cpauk.ReconfigureResult {
	<-s.release
	s.record("reconfigure")
	return cpauk.ReconfigureResult{Applied: true}
}

func (s *blockingAnalyticsService) UpdateKeyLifecycle(_ context.Context, configured, _ []string) error {
	s.record("lifecycle:" + configured[0][:8])
	return nil
}

// Model registration runs after the reload callback, so a slow analytics
// backend must not hold the reload up.
func TestConfigReloadDoesNotWaitForAnalytics(t *testing.T) {
	s := NewServer(&config.Config{AuthDir: t.TempDir()}, nil, nil, filepath.Join(t.TempDir(), "config.yaml"))
	defer s.Stop(context.Background())
	<-s.queueAnalyticsSync(func() {})
	analytics := &blockingAnalyticsService{release: make(chan struct{})}
	s.analytics = analytics

	var want []string
	for _, key := range []string{"sk-first-reload-key", "sk-second-reload-key"} {
		cfg := &config.Config{AuthDir: t.TempDir()}
		cfg.APIKeys = []config.APIKeyEntry{{Key: key}}
		want = append(want, "reconfigure", "lifecycle:"+config.APIKeyID(key)[:8])
		reloaded := make(chan bool, 1)
		go func() { reloaded <- s.UpdateClientsContext(context.Background(), cfg) }()
		select {
		case ok := <-reloaded:
			if !ok {
				t.Fatal("reload was rejected")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("reload waited on a blocked analytics backend")
		}
	}

	close(analytics.release)
	<-s.queueAnalyticsSync(func() {})
	analytics.mu.Lock()
	defer analytics.mu.Unlock()
	if !slices.Equal(analytics.calls, want) {
		t.Fatalf("analytics calls = %v, want %v", analytics.calls, want)
	}
}
