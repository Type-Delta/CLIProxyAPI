package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuotaHistoryThrottleRangeAndRetention(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(ctx) }()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	capacity := 200.0
	sample := QuotaHistorySample{Provider: "claude", RemainingPercent: 50, Capacity: &capacity, CredentialID: strings.Repeat("a", 64), WindowID: "weekly", Source: "cpamc"}
	for _, offset := range []time.Duration{0, time.Minute, 5 * time.Minute} {
		if err := db.RecordQuotaHistory(ctx, []QuotaHistorySample{sample}, now.Add(offset)); err != nil {
			t.Fatal(err)
		}
	}
	changed := sample
	changed.RemainingPercent = 25
	if err := db.RecordQuotaHistory(ctx, []QuotaHistorySample{changed}, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	other := sample
	other.CredentialID = strings.Repeat("b", 64)
	if err := db.RecordQuotaHistory(ctx, []QuotaHistorySample{other}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := db.QuotaHistory(ctx, "claude", now, now.Add(6*time.Minute))
	if err != nil || len(got) != 3 || got[0].RemainingPercent != 50 || got[0].Capacity == nil || *got[0].Capacity != 200 {
		t.Fatalf("samples=%+v err=%v", got, err)
	}
	got, err = db.QuotaHistory(ctx, "claude", now.Add(2*time.Minute), now.Add(5*time.Minute))
	if err != nil || len(got) != 0 {
		t.Fatalf("exclusive range=%+v err=%v", got, err)
	}
	if err := db.RecordQuotaHistory(ctx, []QuotaHistorySample{sample}, now.Add(91*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err = db.QuotaHistory(ctx, "claude", now, now.Add(92*24*time.Hour))
	if err != nil || len(got) != 1 {
		t.Fatalf("retained=%+v err=%v", got, err)
	}
}
