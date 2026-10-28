package chstore

import (
	"context"
	"errors"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestR2LateOverflowDoesNotPoisonFreshIntake(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rate := model.NanoUSD(math.MaxInt64/2 + 1)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "large", Model: "large", InputPerMillion: &rate, OutputPerMillion: &rate, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "manual"}}}
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: book})
	e := loadFixtureEvents(t)[0]
	e.RequestedAt = time.Date(2026, 8, 10, 4, 5, 0, 0, time.UTC)
	e.Model = "large"
	e.ProviderCredits = nil
	e.Tokens = model.TokenUsage{Input: 1000000, Total: 1000000, Schema: "normalized-v1", Quality: model.TokenQualityExact}
	if err := s.WriteBatch(ctx, []model.Event{e}); err != nil {
		t.Fatal(err)
	}
	cutoff := e.RequestedAt.Add(48 * time.Hour)
	if _, err := s.ApplyRetention(ctx, cutoff, 1000); err != nil {
		t.Fatal(err)
	}
	e.AttemptID = fmt.Sprintf("%032x", 9101)
	overflow := s.WriteBatch(ctx, []model.Event{e})
	if overflow == nil {
		t.Fatal("expected overflow")
	}
	t.Logf("late overflow: %v", overflow)
	fresh := loadFixtureEvents(t)[0]
	fresh.RequestedAt = cutoff.Add(time.Hour)
	fresh.AttemptID = fmt.Sprintf("%032x", 9102)
	if err := s.WriteBatch(ctx, []model.Event{fresh}); err != nil {
		t.Errorf("unrelated fresh intake blocked by pending overflowing generation: %v", err)
	}
	if _, err := s.ApplyRetention(ctx, cutoff.Add(time.Hour), 1000); err != nil {
		t.Errorf("subsequent retention blocked: %v", err)
	}
}

func TestR2RollbackOverBudgetClearsCheckpoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	e := loadFixtureEvents(t)[0]
	if _, err := s.WriteImportBatch(ctx, []model.Event{e}, "r2-import"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveImportCheckpoint(ctx, "r2-import", "test", "fingerprint", 1, 1, []byte("finished"), true, [5]int64{1, 1, 1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.config.MaxStorageBytes = 1
	s.mu.Unlock()
	removed, err := s.RollbackImport(ctx, "r2-import")
	var left uint64
	if qerr := s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table("analytics_events")+" FINAL WHERE is_deleted=0 AND import_batch_id='r2-import'").Scan(&left); qerr != nil {
		t.Fatal(qerr)
	}
	_, checkpointPresent, qerr := s.LoadImportCheckpoint(ctx, "r2-import")
	if qerr != nil {
		t.Fatal(qerr)
	}
	t.Logf("rollback removed=%d err=%v raw_left=%d completed_checkpoint_present=%v", removed, err, left, checkpointPresent)
	if err != nil || checkpointPresent {
		t.Errorf("space-freeing rollback did not finish its checkpoint cleanup")
	}
}

func TestR2OldPendingLateRowsWithoutPlanRecover(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	e := loadFixtureEvents(t)[0]
	e.RequestedAt = time.Date(2026, 8, 10, 4, 5, 0, 0, time.UTC)
	if _, err := s.ApplyRetention(ctx, e.RequestedAt.Add(48*time.Hour), 1000); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("persisted old-format pending source")
	failCtx := context.WithValue(ctx, c4bStepKey{}, func(step string) error {
		if step == "late:source" {
			return injected
		}
		return nil
	})
	if _, err := s.WriteImportBatch(failCtx, []model.Event{e}, "old-import"); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	// Before this fix, pending rows existed without a late_retention plan.
	s.mu.Lock()
	err := s.replaceControl(ctx, "late_retention", nil)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	fresh := loadFixtureEvents(t)[0]
	fresh.RequestedAt = e.RequestedAt.Add(72 * time.Hour)
	fresh.AttemptID = fmt.Sprintf("%032x", 9103)
	if err := s.WriteBatch(ctx, []model.Event{fresh}); err == nil || !strings.Contains(err.Error(), "pre-release databases must be recreated") {
		t.Fatalf("expected actionable pre-release schema error, got %v", err)
	}
}

func TestR2CostLeaderboardTotalOverflow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rate := model.NanoUSD(math.MaxInt64/2 + 1)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "large", Model: "large", InputPerMillion: &rate, OutputPerMillion: &rate, CacheReadMultiplier: "1", CacheCreationMultiplier: "1", Source: "manual"}}}
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: book})
	e := loadFixtureEvents(t)[0]
	e.RequestedAt = time.Date(2026, 8, 10, 4, 5, 0, 0, time.UTC)
	e.Model = "large"
	e.ProviderCredits = nil
	e.Tokens = model.TokenUsage{Input: 1000000, Total: 1000000, Schema: "normalized-v1", Quality: model.TokenQualityExact}
	second := e
	second.AttemptID = fmt.Sprintf("%032x", 9104)
	second.KeyID = fmt.Sprintf("%064x", 9104)
	second.RequestedAt = second.RequestedAt.Add(15 * time.Minute)
	if err := s.WriteBatch(ctx, []model.Event{e, second}); err != nil {
		t.Fatal(err)
	}
	q := model.Query{SchemaVersion: 1, Operation: model.OperationLeaderboard, Start: e.RequestedAt.Add(-time.Hour), End: e.RequestedAt.Add(time.Hour), TimeZone: "UTC", SortBy: model.LeaderboardSortCost}
	page, err := s.Leaderboard(ctx, q)
	if err == nil {
		t.Errorf("cost window sum silently overflowed: rows=%+v", page.Rows)
	} else {
		t.Logf("overflow rejected: %v", err)
	}
}

func TestR2StartupLifecycleOverBudget(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ids := []string{fmt.Sprintf("%064x", 1)}
	if err := s.UpdateKeyLifecycle(context.Background(), ids, nil); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.config.MaxStorageBytes = 1
	s.mu.Unlock()
	if err := s.UpdateKeyLifecycle(context.Background(), ids, nil); err != nil {
		t.Fatalf("existing startup state blocked by budget: %v", err)
	}
}

func TestR2PricingPublicationIsCommitPoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	book := fixturePriceBook()
	book.Rules[0].ID = "published-online"
	canceled, cancel := context.WithCancel(ctx)
	defer cancel()
	canceled = context.WithValue(canceled, c4bStepKey{}, func(step string) error {
		if step == "control:published" {
			cancel()
		}
		return nil
	})
	if _, err := s.UpdatePriceBook(canceled, book); err != nil {
		t.Fatalf("published pricing reported failure: %v", err)
	}
	if s.config.PriceBook.Rules[0].ID != "published-online" {
		t.Fatal("writer retained old pricing")
	}
	e := loadFixtureEvents(t)[0]
	if err := s.WriteBatch(ctx, []model.Event{e}); err != nil {
		t.Fatal(err)
	}
	got, err := s.loadEvents(ctx, "attempt_id=?", e.AttemptID)
	if err != nil || got[0].PriceRuleID != "published-online" {
		t.Fatalf("writer pricing %+v %v", got, err)
	}
}

func TestR2AbandonLegacyImport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	impl, ok := any(s).(interface {
		AbandonLegacyImport(context.Context, string) error
	})
	if !ok {
		t.Fatal("missing abandonment operation")
	}
	if err := s.db.SetJob(ctx, c4bLegacyJob, "running", ""); err != nil {
		t.Fatal(err)
	}
	if err := impl.AbandonLegacyImport(ctx, "source unavailable"); err == nil {
		t.Fatal("abandoned running worker")
	}
	if err := s.db.SetJob(ctx, c4bLegacyJob, "failed", "bad source"); err != nil {
		t.Fatal(err)
	}
	if err := impl.AbandonLegacyImport(ctx, "source unavailable"); err != nil {
		t.Fatal(err)
	}
	job, _, err := s.db.Job(ctx, c4bLegacyJob)
	if err != nil || job.State != "abandoned" || !strings.Contains(job.Detail, "source unavailable") || job.UpdatedAt.IsZero() {
		t.Fatalf("job %+v %v", job, err)
	}
	if _, err := s.ApplyRetention(ctx, time.Now().UTC(), 1000); err != nil {
		t.Fatal(err)
	}
	if err := RunLegacyImport(ctx, s, "missing"); err != nil {
		t.Fatal(err)
	}
	job, _, _ = s.db.Job(ctx, c4bLegacyJob)
	if job.State != "abandoned" {
		t.Fatalf("terminal state changed: %+v", job)
	}
}

func TestR2CloseHonorsCanceledContextWithReader(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.mu.RLock()
	done := make(chan error, 1)
	go func() { done <- s.Close(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("close=%v", err)
		}
	case <-time.After(time.Second):
		t.Error("Close blocked behind reader despite cancellation")
	}
	s.mu.RUnlock()
	if s.db == nil {
		t.Fatal("canceled close detached store")
	}
}
