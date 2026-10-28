package chstore

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestQuotaHistoryThrottleRangeAndRetention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
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

func TestC3QuotaHistoryRejectsInvalidBatchWithoutPublishing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	good := QuotaHistorySample{Provider: "claude", CredentialID: strings.Repeat("a", 64), WindowID: "weekly", RemainingPercent: 50, Source: "cpamc"}
	old := good
	old.ObservedAt = now.Add(-91 * 24 * time.Hour)
	db.mu.Lock()
	err := db.appendQuotaHistoryRows(ctx, []quotaHistoryRow{old})
	db.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.CredentialID = "raw-secret"
	if err := db.RecordQuotaHistory(ctx, []QuotaHistorySample{good, bad}, now); err == nil {
		t.Fatal("invalid batch accepted")
	}
	got, err := db.QuotaHistory(ctx, "claude", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(got) != 0 {
		t.Fatalf("partial publication %+v %v", got, err)
	}
	oldRows, err := db.QuotaHistory(ctx, "claude", old.ObservedAt.Add(-time.Second), old.ObservedAt.Add(time.Second))
	if err != nil || len(oldRows) != 1 {
		t.Fatalf("invalid batch pruned history %+v %v", oldRows, err)
	}
}

func TestC3QuotaHistoryAppendRetryAndThrottle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 123, time.UTC)
	id := strings.Repeat("c", 64)
	reset := at.Add(time.Hour)
	period, capacity := 24.0, 200.0
	sample := quotaHistoryRow{Provider: "codex", CredentialID: id, WindowID: "daily", ObservedAt: at, RemainingPercent: 50, ResetAt: &reset, PeriodHours: &period, Capacity: &capacity, Source: "cpamc"}
	s.mu.Lock()
	err := s.appendQuotaHistoryRows(ctx, []quotaHistoryRow{sample, sample})
	if err == nil {
		err = s.appendQuotaHistoryRows(ctx, []quotaHistoryRow{sample})
	}
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordQuotaHistory(ctx, []QuotaHistorySample{sample}, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordQuotaHistory(ctx, []QuotaHistorySample{sample}, at.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordQuotaHistory(ctx, []QuotaHistorySample{sample}, at.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := s.QuotaHistory(ctx, "codex", at.Add(-time.Second), at.Add(time.Hour))
	if err != nil || len(got) != 2 || !got[0].ObservedAt.Equal(at) || got[0].ResetAt == nil || !got[0].ResetAt.Equal(reset) || got[0].PeriodHours == nil || *got[0].PeriodHours != period || got[0].Capacity == nil || *got[0].Capacity != capacity || !got[1].ObservedAt.Equal(at.Add(5*time.Minute)) {
		t.Fatalf("samples %+v %v", got, err)
	}
}
func TestC3QuotaHistoryProviderCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	id := strings.Repeat("a", 64)
	query := "INSERT INTO " + s.table("analytics_quota_history") + " (provider,credential_id,window_id,observed_at_ns,remaining_percent,reset_at_ns,period_hours,plan_id,capacity,source,row_id) SELECT 'codex',?,'daily',?+toInt64(number),50,NULL,NULL,'',NULL,'cpamc',toString(number) FROM numbers(100001)"
	if err := s.db.Conn().Exec(ctx, query, id, at.Add(-time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	sample := QuotaHistorySample{Provider: "codex", CredentialID: id, WindowID: "daily", RemainingPercent: 25, Source: "cpamc"}
	if err := s.RecordQuotaHistory(ctx, []QuotaHistorySample{sample}, at); err != nil {
		t.Fatal(err)
	}
	var count uint64
	var oldest int64
	if err := s.db.Conn().QueryRow(ctx, "SELECT count(),min(observed_at_ns) FROM "+s.table("analytics_quota_history")+" WHERE provider='codex'").Scan(&count, &oldest); err != nil {
		t.Fatal(err)
	}
	if count != 100000 || oldest != at.Add(-time.Hour).UnixNano()+2 {
		t.Fatalf("count %d oldest %d", count, oldest)
	}
}

func TestC3QuotaHistoryExpiryBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-90 * 24 * time.Hour)
	sample := quotaHistoryRow{Provider: "codex", CredentialID: strings.Repeat("a", 64), WindowID: "daily", RemainingPercent: 50, Source: "cpamc"}
	old := sample
	old.ObservedAt = cutoff.Add(-time.Nanosecond)
	sample.ObservedAt = cutoff
	s.mu.Lock()
	err := s.appendQuotaHistoryRows(ctx, []quotaHistoryRow{old, sample})
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordQuotaHistory(ctx, nil, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.QuotaHistory(ctx, "codex", cutoff.Add(-time.Second), cutoff.Add(time.Second))
	if err != nil || len(got) != 1 || !got[0].ObservedAt.Equal(cutoff) {
		t.Fatalf("expiry boundary %+v %v", got, err)
	}
}
