package chstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestPricingMissingGroupsModelProviderAndRangeFacts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	start := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	events := []model.Event{
		c3V2Event("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "11111111111111111111111111111111", "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", start.Add(time.Minute), true, nil, 0, 0, 10, 20),
		c3V2Event("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "22222222222222222222222222222222", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", start.Add(2*time.Minute), true, nil, 0, 0, 10, 20),
	}
	for index := range events {
		events[index].Provider = "missing-provider"
		events[index].Model = "missing-model"
	}
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}

	missing, err := database.PricingMissing(ctx, model.Range{Start: start, End: start.Add(time.Hour), TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 {
		t.Fatalf("missing pricing groups = %+v", missing)
	}
	entry := missing[0]
	if entry.Model != "missing-model" || entry.Provider != "missing-provider" || !entry.FirstSeen.Equal(events[0].RequestedAt) || entry.Requests != 2 || entry.UnpricedTokens != 400 {
		t.Fatalf("missing pricing entry = %+v", entry)
	}
}

func TestPricingMissingRejectsPartialRetainedBuckets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	events := loadFixtureEvents(t)
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ApplyRetention(ctx, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 100); err != nil {
		t.Fatal(err)
	}
	_, err := database.PricingMissing(ctx, model.Range{
		Start: time.Date(2026, 8, 31, 4, 30, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 31, 5, 0, 0, 0, time.UTC), TimeZone: "UTC",
	})
	if !errors.Is(err, ErrRetainedRangePartial) {
		t.Fatalf("partial retained pricing-missing error = %v, want ErrRetainedRangePartial", err)
	}
}
