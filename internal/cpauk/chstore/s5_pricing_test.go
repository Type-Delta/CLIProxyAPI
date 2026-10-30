package chstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestS5PricingMissingFixedWindowRetainedBoundary(t *testing.T) {
	for _, fixture := range []struct {
		zone  string
		daily bool
	}{{"UTC", false}, {"Asia/Bangkok", true}, {"America/New_York", true}} {
		t.Run(fixture.zone, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), RetentionTimeZone: fixture.zone, MaxStorageBytes: 64 << 20})
			end := time.Date(2026, 10, 30, 12, 34, 56, 0, time.UTC)
			start := end.Add(-model.MaxQueryRangeDays * 24 * time.Hour)
			times := []time.Time{start, start.Add(-time.Minute), start.Add(-48 * time.Hour), end.Add(time.Hour)}
			events := make([]model.Event, len(times))
			for i, at := range times {
				events[i] = c2r2V2Event(model.KeyID(at.String())[:32], model.KeyID(at.String())[:32], model.KeyID("s5"), at, true, nil, 0, 0, 10, 20)
			}
			if err := s.WriteBatch(ctx, events); err != nil {
				t.Fatal(err)
			}
			hourly := time.Time{}
			if fixture.daily {
				hourly = start.Add(48 * time.Hour)
			}
			if _, err := s.ApplyRetentionPolicy(ctx, start.Add(72*time.Hour), hourly, 1000); err != nil {
				t.Fatal(err)
			}
			_, oldErr := s.PricingMissing(ctx, model.Range{Start: start.Add(time.Second), End: end, TimeZone: fixture.zone})
			if !errors.Is(oldErr, ErrRetainedRangePartial) {
				t.Fatalf("unaligned explicit range error = %v", oldErr)
			}
			got, err := s.PricingMissing(ctx, model.Range{Start: start, End: end, TimeZone: "UTC"})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].UnpricedTokens != 400 || got[0].Requests != 2 {
				t.Fatalf("missing = %+v", got)
			}
		})
	}
}
