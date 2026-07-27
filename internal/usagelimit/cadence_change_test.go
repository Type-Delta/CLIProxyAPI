package usagelimit

import (
	"path/filepath"
	"testing"
	"time"
)

// TestCadenceChangeAcrossRestartDoesNotFreezeWindow covers a counter persisted
// under one cadence and restored under another. Window ids from different
// cadences are unrelated numbers (a weekly id such as 202630 is far larger than
// a monthly id such as 24319), so a naive forward-only comparison would leave
// the window permanently stuck: never resetting, and blocking the key forever
// if it was already at its limit.
func TestCadenceChangeAcrossRestartDoesNotFreezeWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-limits.json")
	const key = "team-a-key"
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)

	weekly := NewTracker()
	weekly.SetLimits(map[string]Limits{key: {MaxRequests: 2, Resets: PeriodWeekly}})
	for i := 0; i < 2; i++ {
		if decision := weekly.Allow(key, now); !decision.Allowed {
			t.Fatalf("weekly request %d denied: %+v", i, decision)
		}
	}
	if decision := weekly.Allow(key, now); decision.Allowed {
		t.Fatal("weekly limit was not enforced")
	}
	if errFlush := weekly.Flush(path); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}

	// Restart with the cadence switched to monthly.
	restored := NewTracker()
	if errLoad := restored.LoadFrom(path); errLoad != nil {
		t.Fatalf("LoadFrom() error = %v", errLoad)
	}
	restored.SetLimits(map[string]Limits{key: {MaxRequests: 2, Resets: PeriodMonthly}})

	decision := restored.Allow(key, now)
	if !decision.Allowed {
		t.Fatalf("cadence change left the window frozen; key is permanently blocked: %+v", decision)
	}

	snapshot := restored.Snapshot(key, now)
	if snapshot == nil {
		t.Fatal("Snapshot() = nil, want a snapshot")
	}
	if snapshot.Resets != "monthly" {
		t.Fatalf("Resets = %q, want monthly", snapshot.Resets)
	}
	if snapshot.RequestsUsed != 1 {
		t.Fatalf("RequestsUsed = %d, want 1 (counters restart on a cadence change)", snapshot.RequestsUsed)
	}
	if snapshot.ResetAt == nil {
		t.Fatal("ResetAt = nil, want the next monthly boundary")
	}
	if want := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC); !snapshot.ResetAt.Equal(want) {
		t.Fatalf("ResetAt = %s, want %s", snapshot.ResetAt, want)
	}
}

// TestCadenceChangeInMemoryResetsCounters covers the same switch without a
// restart, which SetLimits already guards; both paths must behave identically.
func TestCadenceChangeInMemoryResetsCounters(t *testing.T) {
	const key = "team-a-key"
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)

	tracker := NewTracker()
	tracker.SetLimits(map[string]Limits{key: {MaxRequests: 1, Resets: PeriodWeekly}})
	if decision := tracker.Allow(key, now); !decision.Allowed {
		t.Fatalf("first request denied: %+v", decision)
	}
	if decision := tracker.Allow(key, now); decision.Allowed {
		t.Fatal("weekly limit was not enforced")
	}

	tracker.SetLimits(map[string]Limits{key: {MaxRequests: 1, Resets: PeriodMonthly}})
	if decision := tracker.Allow(key, now); !decision.Allowed {
		t.Fatalf("changing cadence left the key blocked: %+v", decision)
	}
}

// TestPersistedCadenceRoundTrips keeps a matching cadence intact across a
// restart, so the guard cannot be implemented by simply discarding everything.
func TestPersistedCadenceRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-limits.json")
	const key = "team-a-key"
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	limits := map[string]Limits{key: {MaxRequests: 2, Resets: PeriodMonthly}}

	first := NewTracker()
	first.SetLimits(limits)
	if decision := first.Allow(key, now); !decision.Allowed {
		t.Fatalf("first request denied: %+v", decision)
	}
	if errFlush := first.Flush(path); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}

	restored := NewTracker()
	if errLoad := restored.LoadFrom(path); errLoad != nil {
		t.Fatalf("LoadFrom() error = %v", errLoad)
	}
	restored.SetLimits(limits)

	snapshot := restored.Snapshot(key, now)
	if snapshot == nil {
		t.Fatal("Snapshot() = nil, want a snapshot")
	}
	if snapshot.RequestsUsed != 1 {
		t.Fatalf("RequestsUsed = %d, want 1 restored from disk", snapshot.RequestsUsed)
	}
}
