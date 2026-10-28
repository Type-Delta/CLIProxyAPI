package chstore

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestI2LegacyImportServesReadsAndIntakeBetweenChunks(t *testing.T) {
	t.Parallel()
	cfg, events := c4bLegacyFixture(t, false)
	s := openTestStore(t, cfg)
	paused, release := make(chan struct{}), make(chan struct{})
	var pauseOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	ctx := context.WithValue(context.Background(), c4bStepKey{}, func(step string) error {
		if step == "legacy:yield" {
			pauseOnce.Do(func() { close(paused); <-release })
		}
		return nil
	})
	done := make(chan error, 1)
	go func() { done <- RunLegacyImport(ctx, s, cfg.Path) }()
	select {
	case <-paused:
	case <-time.After(30 * time.Second):
		t.Fatal("import did not yield")
	}
	event := events[0]
	event.AttemptID, event.ProxyRequestID = strings.Repeat("f", 32), strings.Repeat("e", 32)
	event.RequestedAt = time.Now().UTC()
	query := model.Query{SchemaVersion: 1, Operation: model.OperationSummary, Start: events[0].RequestedAt.Add(-time.Hour), End: event.RequestedAt.Add(time.Hour), TimeZone: "UTC"}
	online := make(chan error, 1)
	go func() {
		if err := s.WriteBatch(context.Background(), []model.Event{event}); err != nil {
			online <- err
			return
		}
		_, err := s.Summary(context.Background(), query)
		online <- err
	}()
	select {
	case err := <-online:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("analytics blocked behind legacy import")
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	summary, err := s.Summary(context.Background(), query)
	if err != nil || summary.UpstreamAttempts != 4 {
		t.Fatalf("summary %+v err %v", summary, err)
	}
}
