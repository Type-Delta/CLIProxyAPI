package chstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestR3RejectedGenerationCrashDoesNotLeakPlan(t *testing.T) {
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
	before := c4bVisible(t, s)
	e.AttemptID = fmt.Sprintf("%032x", 994001)
	hit := false
	crashCtx := context.WithValue(ctx, c4bStepKey{}, func(step string) error {
		if step == "late:rejected:source-deleted" {
			hit = true
			return errors.New("simulated crash after source delete")
		}
		return nil
	})
	if err := s.WriteBatch(crashCtx, []model.Event{e}); err == nil {
		t.Fatal("expected rejected overflow")
	}
	if !hit {
		t.Fatal("crash boundary not reached")
	}
	after := c4bVisible(t, s)
	if before.Attempts != after.Attempts {
		t.Fatalf("committed baseline lost: before=%+v after=%+v", before, after)
	}
	fresh := loadFixtureEvents(t)[0]
	fresh.RequestedAt = cutoff.Add(time.Hour)
	fresh.AttemptID = fmt.Sprintf("%032x", 994002)
	if err := s.WriteBatch(ctx, []model.Event{fresh}); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	plans, err := s.loadControl(ctx, "late_retention")
	s.mu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) > 0 {
		t.Fatalf("baseline preserved and fresh intake works, but %d orphan rejected-generation plan remains permanently", len(plans))
	}
}

func TestR3LifecycleChargesOnlyGrowth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	ids := make([]string, 10000)
	for i := range ids {
		ids[i] = fmt.Sprintf("%064x", i+1)
	}
	if err := s.UpdateKeyLifecycle(ctx, ids, nil); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.config.MaxStorageBytes = 1
	s.mu.Unlock()
	if err := s.UpdateKeyLifecycle(ctx, ids, nil); err != nil {
		t.Fatalf("existing startup state refused over budget: %v", err)
	}
	if err := s.UpdateKeyLifecycle(ctx, nil, ids[:1]); err != nil {
		t.Fatalf("existing lifecycle transitions refused over budget: %v", err)
	}
	if err := s.UpdateKeyLifecycle(ctx, []string{fmt.Sprintf("%064x", 10001)}, nil); !errors.Is(err, ErrStorageQuota) {
		t.Fatalf("new lifecycle growth under one-byte budget: %v", err)
	}
	s.mu.RLock()
	values, err := s.loadControl(ctx, "key_lifecycle")
	s.mu.RUnlock()
	if err != nil || len(values) != len(ids) {
		t.Fatalf("persisted lifecycle rows=%d err=%v", len(values), err)
	}
	// The ordinary quota path must reject initial growth too, not just merged IDs.
	fresh := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "a.db"), MaxStorageBytes: 1})
	if err := fresh.UpdateKeyLifecycle(ctx, ids[:1], nil); !errors.Is(err, ErrStorageQuota) {
		t.Fatalf("initial lifecycle growth bypassed budget: %v", err)
	}
}
