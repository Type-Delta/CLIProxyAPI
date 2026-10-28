package chstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

func TestReviewLateFractionalBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, RetentionTimeZone: "Asia/Kathmandu", PriceBook: fixturePriceBook()})
	cutoff := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if _, err := s.ApplyRetention(ctx, cutoff, 1000); err != nil {
		t.Fatal(err)
	}
	e := loadFixtureEvents(t)[0]
	e.RequestedAt = cutoff.Add(-5 * time.Minute)
	if n, err := s.WriteImportBatch(ctx, []model.Event{e}, "late"); err != nil || n != 1 {
		t.Fatalf("valid late event rejected: inserted=%d error=%v raw_floor=%v", n, err, s.RetentionCutoff())
	}
}

func TestReviewMoneySumOverflow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rate := model.NanoUSD(math.MaxInt64/2 + 1)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "large", Model: "large", InputPerMillion: &rate, OutputPerMillion: &rate, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "manual"}}}
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: book})
	e := loadFixtureEvents(t)[0]
	e.Model = "large"
	e.Tokens = model.TokenUsage{Input: 1000000, Total: 1000000, Schema: "normalized-v1", Quality: model.TokenQualityExact}
	e.ProviderCredits = nil
	events := []model.Event{e, e}
	// Keep each SQLite compact bucket in range, then overflow across buckets.
	e.RequestedAt = e.RequestedAt.Truncate(time.Hour).Add(time.Minute)
	events[0].RequestedAt = e.RequestedAt
	events[1].RequestedAt = e.RequestedAt.Add(15 * time.Minute)
	events[1].AttemptID = fmt.Sprintf("%032x", 9001)
	if err := s.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	q := model.Query{SchemaVersion: 1, Operation: model.OperationSummary, Start: e.RequestedAt.Add(-time.Hour), End: e.RequestedAt.Add(time.Hour), TimeZone: "UTC"}
	sum, err := s.Summary(ctx, q)
	if err == nil {
		t.Fatalf("overflow was accepted as known_cost_nano=%d, USD=%s", int64(sum.KnownCost), sum.KnownCost.String())
	}
	legacy, err := store.Open(ctx, Config{Path: filepath.Join(t.TempDir(), "overflow.db"), MaxStorageBytes: 64 << 20, PriceBook: book})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = legacy.Close(ctx) }()
	if err = legacy.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.Summary(ctx, q); err == nil {
		t.Fatal("SQLite overflow reference unexpectedly accepted sum")
	}
	checks := map[string]func() error{
		"timeseries": func() error {
			q := q
			q.Operation = model.OperationTimeseries
			q.BucketWidth = "1h"
			_, err := s.Timeseries(ctx, q)
			return err
		},
		"dimensions": func() error {
			q := q
			q.Operation = model.OperationDimensions
			q.Dimension = "model"
			_, err := s.Dimensions(ctx, q)
			return err
		},
		"key catalog": func() error {
			q := q
			q.Operation = model.OperationDimensions
			q.Dimension = "key"
			_, err := s.KeyCatalog(ctx, q)
			return err
		},
		"cost series": func() error {
			q := q
			q.Operation = model.OperationModelCostSeries
			q.SchemaVersion = 2
			q.BucketWidth = "1h"
			q.Filters = map[string]json.RawMessage{"model": json.RawMessage(`["large"]`)}
			_, err := s.ModelCostSeries(ctx, q)
			return err
		},
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			err := check()
			if err == nil {
				t.Fatal("overflow was accepted")
			}
			if !strings.Contains(err.Error(), "integer overflow") && !strings.Contains(err.Error(), "cannot be safely converted") {
				t.Fatalf("expected numeric overflow, got %v", err)
			}
		})
	}

}

func TestReviewLegacyFailureThenRetentionThenRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 128 << 20, PriceBook: fixturePriceBook()}
	legacy, err := store.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	fixture := loadFixtureEvents(t)[0]
	fixture.RequestedAt = time.Date(2026, 8, 10, 4, 5, 0, 0, time.UTC)
	events := make([]model.Event, 1003)
	for i := range events {
		events[i] = fixture
		events[i].AttemptID = fmt.Sprintf("%032x", i+1)
		events[i].ProxyRequestID = fmt.Sprintf("%032x", i+1)
	}
	if err = legacy.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	if err = legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, cfg)
	interrupted := errors.New("simulated failure after first chunk")
	failureCtx := context.WithValue(ctx, c4bStepKey{}, func(step string) error {
		if step == "legacy:events-checkpoint" {
			return interrupted
		}
		return nil
	})
	if err = RunLegacyImport(failureCtx, s, cfg.Path); !errors.Is(err, interrupted) {
		t.Fatalf("failure err=%v", err)
	}
	if _, err = s.ApplyRetention(ctx, fixture.RequestedAt.AddDate(0, 0, 2), 1000); err == nil {
		t.Fatal("retention accepted failed import")
	}
	if err = RunLegacyImport(ctx, s, cfg.Path); err != nil {
		t.Fatal(err)
	}
	got := c4bVisible(t, s)
	if got.Attempts != int64(len(events)) {
		t.Fatalf("successful retry exposes %d attempts, source has %d", got.Attempts, len(events))
	}
}

func TestReviewLegacyOverwritesOnlineManualPricing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg, _ := c4bLegacyFixture(t, false)
	s := openTestStore(t, cfg)
	changed := false
	importCtx := context.WithValue(ctx, c4bStepKey{}, func(step string) error {
		if step == "legacy:yield" && !changed {
			changed = true
			b := fixturePriceBook()
			b.Rules[0].ID = "new-online-manual"
			_, err := s.UpdatePriceBook(ctx, b)
			return err
		}
		return nil
	})
	if err := RunLegacyImport(importCtx, s, cfg.Path); err != nil {
		t.Fatal(err)
	}
	b, err := s.PriceBook(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range b.Rules {
		if r.ID == "new-online-manual" {
			return
		}
	}
	t.Fatalf("acknowledged online manual price was overwritten; final rules=%+v", b.Rules)
}

func TestReviewLateSkippedMidnight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	zone := "America/Sao_Paulo"
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, RetentionTimeZone: zone, PriceBook: fixturePriceBook()})
	cutoff := time.Date(2018, 11, 6, 3, 0, 0, 0, time.UTC)
	if _, err := s.ApplyRetentionPolicy(ctx, cutoff, cutoff, 1000); err != nil {
		t.Fatal(err)
	}
	e := loadFixtureEvents(t)[0]
	e.RequestedAt = time.Date(2018, 11, 4, 3, 15, 0, 0, time.UTC)
	if _, err := s.WriteImportBatch(ctx, []model.Event{e}, "skip-midnight"); err != nil {
		t.Fatal(err)
	}
	wantStart, wantEnd, err := aggregate.BucketBounds(e.RequestedAt, zone, "1d")
	if err != nil {
		t.Fatal(err)
	}
	var start, end int64
	if err := s.db.Conn().QueryRow(ctx, "SELECT bucket_start_ns,bucket_end_ns FROM "+s.table("analytics_rollups")+" FINAL WHERE is_deleted=0 AND "+s.c4bPublishedPredicate()).Scan(&start, &end); err != nil {
		t.Fatal(err)
	}
	if start != wantStart.UnixNano() || end != wantEnd.UnixNano() {
		t.Fatalf("late daily bucket=%s..%s; canonical=%s..%s", time.Unix(0, start).UTC(), time.Unix(0, end).UTC(), wantStart, wantEnd)
	}
}

func TestReviewLateFractionalDSTTotals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	zone := "Australia/Lord_Howe"
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, RetentionTimeZone: zone, PriceBook: fixturePriceBook()})
	base := loadFixtureEvents(t)[0]
	events := []model.Event{base, base}
	events[0].RequestedAt = time.Date(2026, 4, 4, 14, 45, 0, 0, time.UTC)
	events[1].RequestedAt = time.Date(2026, 4, 4, 15, 15, 0, 0, time.UTC)
	events[1].AttemptID = fmt.Sprintf("%032x", 9002)
	if err := s.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	if _, err := s.ApplyRetention(ctx, cutoff, 1000); err != nil {
		t.Fatal(err)
	}
	before := c4bVisible(t, s)
	late := events[1]
	late.AttemptID = fmt.Sprintf("%032x", 9003)
	late.RequestedAt = late.RequestedAt.Add(5 * time.Minute)
	if _, err := s.WriteImportBatch(ctx, []model.Event{late}, "same-owner"); err != nil {
		t.Fatal(err)
	}
	// Use a live late row to merge into the native baseline bucket too.
	late.AttemptID = fmt.Sprintf("%032x", 9004)
	if err := s.WriteBatch(ctx, []model.Event{late}); err != nil {
		t.Fatal(err)
	}
	after := c4bVisible(t, s)
	if after.Attempts != before.Attempts+2 {
		t.Fatalf("late DST writes lost history: before=%+v after=%+v want_attempts=%d", before, after, before.Attempts+2)
	}
}

func TestReviewControlsRespectStorageBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 1})
	if _, err := s.UpdatePriceBook(ctx, fixturePriceBook()); !errors.Is(err, ErrStorageQuota) {
		t.Fatalf("pricing write succeeded past 1-byte analytics budget: %v", err)
	}
}

func TestReviewNativeRetentionMoneyOverflow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rate := model.NanoUSD(math.MaxInt64/2 + 1)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "large", Model: "large", InputPerMillion: &rate, OutputPerMillion: &rate, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "manual"}}}
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: book})
	e := loadFixtureEvents(t)[0]
	e.Model = "large"
	e.Tokens = model.TokenUsage{Input: 1000000, Total: 1000000, Schema: "normalized-v1", Quality: model.TokenQualityExact}
	e.ProviderCredits = nil
	events := []model.Event{e, e}
	events[1].AttemptID = fmt.Sprintf("%032x", 9005)
	if err := s.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	cutoff := e.RequestedAt.Add(48 * time.Hour)
	if _, err := s.ApplyRetention(ctx, cutoff, 1000); err == nil {
		t.Fatal("retention accepted overflowing money")
	}
	var raw, heads uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table("analytics_events")+" FINAL WHERE is_deleted=0").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table("analytics_retention_heads")+" FINAL").Scan(&heads); err != nil {
		t.Fatal(err)
	}
	if raw != 2 || heads != 0 || s.RetentionCutoff() != nil {
		t.Fatalf("overflow changed visibility raw=%d heads=%d cutoff=%v", raw, heads, s.RetentionCutoff())
	}
}

func TestReviewDestructiveMaintenanceRequiresCompletedImport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	for _, state := range []string{"pending", "running", "failed"} {
		if err := s.db.SetJob(ctx, c4bLegacyJob, state, ""); err != nil {
			t.Fatal(err)
		}
		checks := map[string]func() error{
			"retention": func() error { _, err := s.ApplyRetention(ctx, time.Now().UTC(), 1000); return err },
			"purge":     func() error { _, err := s.PurgeByKeyID(ctx, fmt.Sprintf("%064x", 1)); return err },
			"rollback":  func() error { _, err := s.RollbackImport(ctx, "batch"); return err },
			"epoch":     func() error { _, err := s.StartNewIdentityEpoch(ctx); return err },
			"restore":   func() error { return s.Restore(ctx, "missing", "missing") },
		}
		for name, check := range checks {
			if err := check(); err == nil {
				t.Fatalf("%s accepted %s import", name, state)
			}
		}
	}
}

func TestReviewControlGenerationsAreReclaimed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	for i := 0; i < 4; i++ {
		b := fixturePriceBook()
		b.Rules[0].ID = fmt.Sprint("rule-", i)
		if _, err := s.UpdatePriceBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	var obsolete uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table("analytics_control")+" WHERE namespace='pricing' AND generation NOT IN (SELECT generation FROM "+s.table("analytics_control_heads")+" FINAL WHERE namespace='pricing')").Scan(&obsolete); err != nil {
		t.Fatal(err)
	}
	if obsolete != 0 {
		t.Fatalf("superseded control rows remain: %d", obsolete)
	}
	b, err := s.PriceBook(ctx)
	if err != nil || b.Rules[0].ID != "rule-3" {
		t.Fatalf("latest pricing lost: %+v %v", b, err)
	}
}

func TestReviewQuotaHistoryRespectsBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 1})
	sample := QuotaHistorySample{Provider: "codex", CredentialID: fmt.Sprintf("%064x", 1), WindowID: "daily", RemainingPercent: 50, Source: "cpamc"}
	if err := s.RecordQuotaHistory(ctx, []QuotaHistorySample{sample}, time.Now().UTC()); !errors.Is(err, ErrStorageQuota) {
		t.Fatalf("quota history accepted over budget: %v", err)
	}
}

func TestReviewLegacyResumeBelowPublishedFloor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg, _ := c4bLegacyFixture(t, false)
	s := openTestStore(t, cfg)
	// Simulate a database advanced by a pre-fix binary before its import resumed.
	if err := s.db.SetJob(ctx, c4bLegacyJob, "done", ""); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.ApplyRetention(ctx, cutoff, 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetJob(ctx, c4bLegacyJob, "failed", ""); err != nil {
		t.Fatal(err)
	}
	if err := RunLegacyImport(ctx, s, cfg.Path); err != nil {
		t.Fatal(err)
	}
	want := c4bSQLiteTotals(t, cfg.Path)
	got := c4bVisible(t, s)
	if got.Attempts != want.Attempts || got.Money != want.Money {
		t.Fatalf("resumed import lost facts or recorded prices: got=%+v want=%+v", got, want)
	}
}

func TestReviewLateBoundsSurviveRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	zone := "Australia/Lord_Howe"
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, RetentionTimeZone: zone, PriceBook: fixturePriceBook()})
	e := loadFixtureEvents(t)[0]
	e.RequestedAt = time.Date(2026, 4, 4, 15, 15, 0, 0, time.UTC)
	if _, err := s.ApplyRetention(ctx, e.RequestedAt.Add(48*time.Hour), 1000); err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("source persisted")
	failure := context.WithValue(ctx, c4bStepKey{}, func(step string) error {
		if step == "late:source" {
			return interrupted
		}
		return nil
	})
	if _, err := s.WriteImportBatch(failure, []model.Event{e}, "recovery"); !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	var expectedStart, expectedEnd int64
	controls, err := s.loadControl(ctx, "late_retention")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range controls {
		var plan []c4bBucket
		if err = json.Unmarshal(raw, &plan); err != nil {
			t.Fatal(err)
		}
		if len(plan) == 1 {
			expectedStart, expectedEnd = plan[0].Start, plan[0].End
		}
	}
	s.mu.Lock()
	err = s.c4bRecoverLate(ctx)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var start, end int64
	if err = s.db.Conn().QueryRow(ctx, "SELECT bucket_start_ns,bucket_end_ns FROM "+s.table("analytics_rollups")+" FINAL WHERE is_deleted=0 AND "+s.c4bPublishedPredicate()).Scan(&start, &end); err != nil {
		t.Fatal(err)
	}
	if expectedEnd == 0 || start != expectedStart || end != expectedEnd {
		t.Fatalf("recovery changed planned bounds: %d..%d want %d..%d", start, end, expectedStart, expectedEnd)
	}
}

func TestReviewLateAndDailyMoneyOverflow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rate := model.NanoUSD(math.MaxInt64/2 + 1)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "large", Model: "large", InputPerMillion: &rate, OutputPerMillion: &rate, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "manual"}}}
	for _, daily := range []bool{false, true} {
		t.Run(fmt.Sprint("daily-", daily), func(t *testing.T) {
			s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: book})
			e := loadFixtureEvents(t)[0]
			e.RequestedAt = time.Date(2026, 8, 10, 4, 5, 0, 0, time.UTC)
			e.Model = "large"
			e.ProviderCredits = nil
			e.Tokens = model.TokenUsage{Input: 1000000, Total: 1000000, Schema: "normalized-v1", Quality: model.TokenQualityExact}
			events := []model.Event{e}
			if daily {
				second := e
				second.RequestedAt = second.RequestedAt.Add(time.Hour)
				second.AttemptID = fmt.Sprintf("%032x", 9006)
				events = append(events, second)
			}
			if err := s.WriteBatch(ctx, events); err != nil {
				t.Fatal(err)
			}
			cutoff := e.RequestedAt.Add(48 * time.Hour)
			if _, err := s.ApplyRetention(ctx, cutoff, 1000); err != nil {
				t.Fatal(err)
			}
			var err error
			if daily {
				_, err = s.ApplyRetentionPolicy(ctx, cutoff, cutoff, 1000)
			} else {
				e.AttemptID = fmt.Sprintf("%032x", 9007)
				err = s.WriteBatch(ctx, []model.Event{e})
			}
			if err == nil || !strings.Contains(err.Error(), "cannot be safely converted") {
				t.Fatalf("expected checked money overflow, got %v", err)
			}
			var sourceCount, publishedDaily uint64
			source := s.table("analytics_events") + " FINAL WHERE is_deleted=0"
			if daily {
				source = s.table("analytics_rollups") + " FINAL WHERE grain='hourly' AND is_deleted=0 AND " + s.c4bPublishedPredicate()
			}
			if err = s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+source).Scan(&sourceCount); err != nil {
				t.Fatal(err)
			}
			expected := uint64(len(events))
			if !daily {
				expected = 0
				if got := c4bVisible(t, s); got.Attempts != 1 {
					t.Fatalf("rejected late write changed committed baseline: %+v", got)
				}
			}
			if sourceCount != expected {
				t.Fatalf("unexpected source count after overflow: %d want %d", sourceCount, expected)
			}
			if daily {
				if err = s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table("analytics_retention_heads")+" FINAL WHERE grain='daily'").Scan(&publishedDaily); err != nil {
					t.Fatal(err)
				}
				if publishedDaily != 0 {
					t.Fatal("published overflowing daily total")
				}
			}
		})
	}
}

func TestReviewLateCanonicalStartWithOverlappingPublishedBucket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	zone := "Australia/Lord_Howe"
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, RetentionTimeZone: zone, PriceBook: fixturePriceBook()})
	e := loadFixtureEvents(t)[0]
	e.RequestedAt = time.Date(2026, 4, 4, 15, 15, 0, 0, time.UTC)
	if err := s.WriteBatch(ctx, []model.Event{e}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyRetention(ctx, e.RequestedAt.Add(48*time.Hour), 1000); err != nil {
		t.Fatal(err)
	}
	e.AttemptID = fmt.Sprintf("%032x", 9010)
	e.RequestedAt = time.Date(2026, 4, 4, 14, 45, 0, 0, time.UTC)
	if _, err := s.WriteImportBatch(ctx, []model.Event{e}, "first-hour"); err != nil {
		t.Fatal(err)
	}
	start, end, err := retainedBucketBounds(e.RequestedAt, zone, false)
	if err != nil {
		t.Fatal(err)
	}
	var gotStart, gotEnd int64
	if err = s.db.Conn().QueryRow(ctx, "SELECT bucket_start_ns,bucket_end_ns FROM "+s.table("analytics_rollups")+" FINAL WHERE import_batch_id='first-hour' AND is_deleted=0 AND "+s.c4bPublishedPredicate()).Scan(&gotStart, &gotEnd); err != nil {
		t.Fatal(err)
	}
	if gotStart != start.UnixNano() || gotEnd != end.UnixNano() {
		t.Fatalf("overlapping bucket changed canonical bounds: %d..%d want %d..%d", gotStart, gotEnd, start.UnixNano(), end.UnixNano())
	}
}
