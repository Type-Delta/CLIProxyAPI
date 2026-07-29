package usagelimit

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestParsePeriod(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  Period
		valid bool
	}{
		{name: "empty", want: PeriodLifetime, valid: true},
		{name: "whitespace", value: " \t", want: PeriodLifetime, valid: true},
		{name: "hourly", value: "hourly", want: PeriodHourly, valid: true},
		{name: "daily", value: "daily", want: PeriodDaily, valid: true},
		{name: "weekly", value: "weekly", want: PeriodWeekly, valid: true},
		{name: "monthly", value: "monthly", want: PeriodMonthly, valid: true},
		{name: "trimmed valid", value: " weekly ", want: PeriodWeekly, valid: true},
		{name: "unknown", value: "yearly"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, errParse := ParsePeriod(tt.value)
			if (errParse == nil) != tt.valid {
				t.Fatalf("ParsePeriod(%q) error = %v, valid = %t", tt.value, errParse, tt.valid)
			}
			if got != tt.want {
				t.Fatalf("ParsePeriod(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestLimitsIsZero(t *testing.T) {
	tests := []struct {
		limits Limits
		want   bool
	}{
		{want: true},
		{limits: Limits{MaxRequests: 1}},
		{limits: Limits{MaxTokens: 1}},
	}
	for _, tt := range tests {
		if got := tt.limits.IsZero(); got != tt.want {
			t.Fatalf("Limits(%+v).IsZero() = %t, want %t", tt.limits, got, tt.want)
		}
	}
}

func TestAllowRequestLimitAndDecision(t *testing.T) {
	now := time.Date(2026, time.July, 25, 10, 15, 30, 0, time.UTC)
	tracker := NewTracker()
	tracker.SetLimits(map[string]Limits{"key": {MaxRequests: 2, Resets: PeriodHourly}})

	for range 2 {
		if decision := tracker.Allow("key", now); !decision.Allowed {
			t.Fatalf("allowed request denied: %+v", decision)
		}
	}

	resetAt := time.Date(2026, time.July, 25, 11, 0, 0, 0, time.UTC)
	want := Decision{
		Allowed: false,
		Metric:  "requests",
		Limit:   2,
		Used:    2,
		Resets:  PeriodHourly,
		ResetAt: &resetAt,
	}
	if got := tracker.Allow("key", now); !reflect.DeepEqual(got, want) {
		t.Fatalf("Allow() = %+v, want %+v", got, want)
	}
}

func TestTokenLimitDeniesOnFollowingRequest(t *testing.T) {
	now := time.Date(2026, time.July, 25, 10, 15, 30, 0, time.UTC)
	tracker := NewTracker()
	tracker.SetLimits(map[string]Limits{"key": {MaxRequests: 10, MaxTokens: 5, Resets: PeriodDaily}})

	if decision := tracker.Allow("key", now); !decision.Allowed {
		t.Fatalf("first request denied: %+v", decision)
	}
	tracker.AddTokens("key", 6, now)

	resetAt := time.Date(2026, time.July, 26, 0, 0, 0, 0, time.UTC)
	want := Decision{
		Allowed: false,
		Metric:  "tokens",
		Limit:   5,
		Used:    6,
		Resets:  PeriodDaily,
		ResetAt: &resetAt,
	}
	if got := tracker.Allow("key", now); !reflect.DeepEqual(got, want) {
		t.Fatalf("Allow() after observed token overshoot = %+v, want %+v", got, want)
	}
	if got := tracker.Snapshot("key", now).RequestsUsed; got != 1 {
		t.Fatalf("denied request changed RequestsUsed to %d, want 1", got)
	}
}

func TestWindowRollover(t *testing.T) {
	tests := []struct {
		name   string
		period Period
		first  time.Time
		next   time.Time
		reset  time.Time
	}{
		{
			name:   "hourly",
			period: PeriodHourly,
			first:  time.Date(2026, time.July, 25, 10, 59, 59, 0, time.UTC),
			next:   time.Date(2026, time.July, 25, 11, 0, 0, 0, time.UTC),
			reset:  time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC),
		},
		{
			name:   "daily",
			period: PeriodDaily,
			first:  time.Date(2026, time.July, 25, 23, 59, 59, 0, time.UTC),
			next:   time.Date(2026, time.July, 26, 0, 0, 0, 0, time.UTC),
			reset:  time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC),
		},
		{
			name:   "weekly",
			period: PeriodWeekly,
			first:  time.Date(2026, time.July, 26, 23, 59, 59, 0, time.UTC),
			next:   time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC),
			reset:  time.Date(2026, time.August, 3, 0, 0, 0, 0, time.UTC),
		},
		{
			name:   "monthly",
			period: PeriodMonthly,
			first:  time.Date(2026, time.July, 31, 23, 59, 59, 0, time.UTC),
			next:   time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
			reset:  time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker()
			tracker.SetLimits(map[string]Limits{"key": {MaxRequests: 1, Resets: tt.period}})
			if decision := tracker.Allow("key", tt.first); !decision.Allowed {
				t.Fatalf("first request denied: %+v", decision)
			}
			if decision := tracker.Allow("key", tt.first); decision.Allowed {
				t.Fatal("second request in window was allowed")
			}
			if decision := tracker.Allow("key", tt.next); !decision.Allowed {
				t.Fatalf("request after rollover denied: %+v", decision)
			}
			snapshot := tracker.Snapshot("key", tt.next)
			if snapshot.RequestsUsed != 1 || snapshot.ResetAt == nil || !snapshot.ResetAt.Equal(tt.reset) {
				t.Fatalf("Snapshot() = %+v, want one request and reset %s", snapshot, tt.reset)
			}
		})
	}
}

func TestWeeklyRolloverAndISOYearBoundary(t *testing.T) {
	tracker := NewTracker()
	tracker.SetLimits(map[string]Limits{"key": {MaxRequests: 1, Resets: PeriodWeekly}})

	start := time.Date(2026, time.December, 28, 0, 0, 0, 0, time.UTC)
	midWeek := time.Date(2027, time.January, 3, 23, 59, 59, 0, time.UTC)
	nextMonday := time.Date(2027, time.January, 4, 0, 0, 0, 0, time.UTC)

	if decision := tracker.Allow("key", start); !decision.Allowed {
		t.Fatalf("start request denied: %+v", decision)
	}
	if decision := tracker.Allow("key", midWeek); decision.Allowed {
		t.Fatal("mid-week request was allowed")
	}
	if decision := tracker.Allow("key", nextMonday); !decision.Allowed {
		t.Fatalf("ISO year boundary rollover denied: %+v", decision)
	}

	snapshot := tracker.Snapshot("key", nextMonday)
	wantReset := time.Date(2027, time.January, 11, 0, 0, 0, 0, time.UTC)
	if snapshot.ResetAt == nil || !snapshot.ResetAt.Equal(wantReset) {
		t.Fatalf("weekly ResetAt = %v, want %s", snapshot.ResetAt, wantReset)
	}
}

func TestMonthlyRolloverAcrossCalendarLengths(t *testing.T) {
	tests := []struct {
		name  string
		first time.Time
		next  time.Time
		reset time.Time
	}{
		{
			name:  "December to January",
			first: time.Date(2026, time.December, 31, 23, 59, 59, 0, time.UTC),
			next:  time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC),
			reset: time.Date(2027, time.February, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:  "28 day February",
			first: time.Date(2025, time.February, 28, 23, 59, 59, 0, time.UTC),
			next:  time.Date(2025, time.March, 1, 0, 0, 0, 0, time.UTC),
			reset: time.Date(2025, time.April, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:  "29 day February",
			first: time.Date(2024, time.February, 29, 23, 59, 59, 0, time.UTC),
			next:  time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC),
			reset: time.Date(2024, time.April, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:  "30 day April",
			first: time.Date(2025, time.April, 30, 23, 59, 59, 0, time.UTC),
			next:  time.Date(2025, time.May, 1, 0, 0, 0, 0, time.UTC),
			reset: time.Date(2025, time.June, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:  "31 day January",
			first: time.Date(2025, time.January, 31, 23, 59, 59, 0, time.UTC),
			next:  time.Date(2025, time.February, 1, 0, 0, 0, 0, time.UTC),
			reset: time.Date(2025, time.March, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker()
			tracker.SetLimits(map[string]Limits{"key": {MaxRequests: 1, Resets: PeriodMonthly}})
			if decision := tracker.Allow("key", tt.first); !decision.Allowed {
				t.Fatalf("first request denied: %+v", decision)
			}
			if decision := tracker.Allow("key", tt.next); !decision.Allowed {
				t.Fatalf("request after month rollover denied: %+v", decision)
			}
			snapshot := tracker.Snapshot("key", tt.next)
			if snapshot.ResetAt == nil || !snapshot.ResetAt.Equal(tt.reset) {
				t.Fatalf("monthly ResetAt = %v, want %s", snapshot.ResetAt, tt.reset)
			}
		})
	}
}

func TestLifetimeNeverResets(t *testing.T) {
	tracker := NewTracker()
	tracker.SetLimits(map[string]Limits{"key": {MaxRequests: 1}})
	first := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	later := time.Date(2027, time.June, 1, 0, 0, 0, 0, time.UTC)

	if decision := tracker.Allow("key", first); !decision.Allowed {
		t.Fatalf("first request denied: %+v", decision)
	}
	decision := tracker.Allow("key", later)
	if decision.Allowed || decision.Resets != PeriodLifetime || decision.ResetAt != nil {
		t.Fatalf("lifetime decision = %+v, want lifetime denial without reset", decision)
	}
	snapshot := tracker.Snapshot("key", later)
	if snapshot.Resets != "lifetime" || snapshot.ResetAt != nil || snapshot.RequestsUsed != 1 {
		t.Fatalf("lifetime Snapshot() = %+v", snapshot)
	}
}

func TestStaleTimesDoNotRewindWindow(t *testing.T) {
	tracker := NewTracker()
	tracker.SetLimits(map[string]Limits{"key": {MaxRequests: 2, MaxTokens: 1_000, Resets: PeriodHourly}})
	previous := time.Date(2026, time.July, 25, 10, 30, 0, 0, time.UTC)
	current := time.Date(2026, time.July, 25, 11, 30, 0, 0, time.UTC)

	if decision := tracker.Allow("key", previous); !decision.Allowed {
		t.Fatalf("previous window request denied: %+v", decision)
	}
	for range 2 {
		if decision := tracker.Allow("key", current); !decision.Allowed {
			t.Fatalf("current window request denied: %+v", decision)
		}
	}
	if decision := tracker.Allow("key", previous); decision.Allowed {
		t.Fatal("backdated Allow rewound the active window")
	}

	tracker.AddTokens("key", 10, previous)
	if decision := tracker.Allow("key", current); decision.Allowed {
		t.Fatal("backdated AddTokens rewound the active window")
	}
	snapshot := tracker.Snapshot("key", current)
	if snapshot.RequestsUsed != 2 || snapshot.TokensUsed != 10 {
		t.Fatalf("stale update Snapshot() = %+v, want current counters", snapshot)
	}
}

func TestUnlimitedKeys(t *testing.T) {
	now := time.Date(2026, time.July, 25, 10, 30, 0, 0, time.UTC)
	tests := []struct {
		name   string
		limits map[string]Limits
		key    string
	}{
		{name: "absent", limits: map[string]Limits{"limited": {MaxRequests: 1}}, key: "absent"},
		{name: "nil map", limits: nil, key: "key"},
		{name: "zero limits", limits: map[string]Limits{"key": {}}, key: "key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker()
			tracker.SetLimits(tt.limits)
			for range 3 {
				if decision := tracker.Allow(tt.key, now); !decision.Allowed {
					t.Fatalf("unlimited request denied: %+v", decision)
				}
			}
			tracker.AddTokens(tt.key, 100, now)
			if snapshot := tracker.Snapshot(tt.key, now); snapshot != nil {
				t.Fatalf("Snapshot() = %+v, want nil", snapshot)
			}
		})
	}
}

func TestSetLimitsPreservesDropsAndResets(t *testing.T) {
	now := time.Date(2026, time.July, 25, 10, 30, 0, 0, time.UTC)
	tracker := NewTracker()
	tracker.SetLimits(map[string]Limits{
		"keep": {MaxRequests: 5, Resets: PeriodHourly},
		"gone": {MaxRequests: 5, Resets: PeriodHourly},
	})
	for range 2 {
		tracker.Allow("keep", now)
	}
	tracker.Allow("gone", now)

	tracker.SetLimits(map[string]Limits{"keep": {MaxRequests: 5, Resets: PeriodHourly}})
	if got := tracker.Snapshot("keep", now).RequestsUsed; got != 2 {
		t.Fatalf("surviving RequestsUsed = %d, want 2", got)
	}
	if snapshot := tracker.Snapshot("gone", now); snapshot != nil {
		t.Fatalf("removed Snapshot() = %+v, want nil", snapshot)
	}

	tracker.SetLimits(map[string]Limits{"keep": {MaxRequests: 5, Resets: PeriodDaily}})
	if got := tracker.Snapshot("keep", now).RequestsUsed; got != 0 {
		t.Fatalf("cadence change RequestsUsed = %d, want 0", got)
	}
}

func TestLoweringLimitTakesEffectImmediately(t *testing.T) {
	now := time.Date(2026, time.July, 25, 10, 30, 0, 0, time.UTC)
	tracker := NewTracker()
	tracker.SetLimits(map[string]Limits{"key": {MaxRequests: 3, Resets: PeriodHourly}})
	tracker.Allow("key", now)
	tracker.Allow("key", now)
	tracker.SetLimits(map[string]Limits{"key": {MaxRequests: 1, Resets: PeriodHourly}})

	if decision := tracker.Allow("key", now); decision.Allowed || decision.Used != 2 || decision.Limit != 1 {
		t.Fatalf("lowered limit decision = %+v", decision)
	}
}

func TestAddTokensSaturatesAndIgnoresNonPositive(t *testing.T) {
	now := time.Date(2026, time.July, 25, 10, 30, 0, 0, time.UTC)
	tracker := NewTracker()
	tracker.SetLimits(map[string]Limits{"key": {MaxTokens: 1, Resets: PeriodHourly}})
	tracker.AddTokens("key", maxInt64-1, now)
	tracker.AddTokens("key", 10, now)
	tracker.AddTokens("key", 0, now)
	tracker.AddTokens("key", -5, now)

	if got := tracker.Snapshot("key", now).TokensUsed; got != maxInt64 {
		t.Fatalf("TokensUsed = %d, want %d", got, maxInt64)
	}
}

func TestResetClearsConfiguredKeyUsage(t *testing.T) {
	tracker := NewTracker()
	tracker.SetLimits(map[string]Limits{
		"limited":   {MaxRequests: 2, MaxTokens: 10, Resets: PeriodDaily},
		"unlimited": {},
	})
	now := time.Date(2026, time.July, 25, 10, 30, 0, 0, time.UTC)
	tracker.Allow("limited", now)
	tracker.AddTokens("limited", 3, now)
	tracker.persisted[keyHash("limited")] = counter{initialized: true}
	tracker.dirty = false

	if !tracker.Reset("limited") {
		t.Fatal("Reset(limited) = false, want true")
	}
	if _, exists := tracker.counters["limited"]; exists {
		t.Fatal("Reset(limited) retained active counter")
	}
	if _, exists := tracker.persisted[keyHash("limited")]; exists {
		t.Fatal("Reset(limited) retained persisted counter")
	}
	if !tracker.dirty {
		t.Fatal("Reset(limited) did not mark tracker dirty")
	}
	if snapshot := tracker.Snapshot("limited", now); snapshot.RequestsUsed != 0 || snapshot.TokensUsed != 0 {
		t.Fatalf("Snapshot after Reset = %+v, want zero usage", snapshot)
	}
	if tracker.Reset("missing") {
		t.Fatal("Reset(missing) = true, want false")
	}
	if tracker.Reset("unlimited") {
		t.Fatal("Reset(unlimited) = true, want false")
	}
}

func TestKeysSorted(t *testing.T) {
	tracker := NewTracker()
	tracker.SetLimits(map[string]Limits{"zebra": {}, "alpha": {}, "middle": {}})
	if got, want := tracker.Keys(), []string{"alpha", "middle", "zebra"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Keys() = %v, want %v", got, want)
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-limits.json")
	key := "super-secret-api-key"
	now := time.Date(2026, time.July, 25, 10, 30, 0, 0, time.UTC)
	limits := map[string]Limits{key: {MaxRequests: 10, MaxTokens: 100, Resets: PeriodDaily}}

	tracker := NewTracker()
	tracker.SetLimits(limits)
	tracker.Allow(key, now)
	tracker.AddTokens(key, 25, now)
	if errFlush := tracker.Flush(path); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}

	contents, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if bytes.Contains(contents, []byte(key)) {
		t.Fatal("persisted usage limits include raw API key")
	}
	// Windows has no Unix permission bits: os.Stat reports 0666 for any writable
	// file regardless of the mode requested, so only assert where it is meaningful.
	if runtime.GOOS != "windows" {
		info, errStat := os.Stat(path)
		if errStat != nil {
			t.Fatalf("Stat() error = %v", errStat)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("snapshot permissions = %o, want 600", info.Mode().Perm())
		}
	}

	restored := NewTracker()
	if errLoad := restored.LoadFrom(path); errLoad != nil {
		t.Fatalf("LoadFrom() error = %v", errLoad)
	}
	restored.SetLimits(limits)
	if got := restored.Snapshot(key, now); got.RequestsUsed != 1 || got.TokensUsed != 25 {
		t.Fatalf("restored Snapshot() = %+v, want one request and 25 tokens", got)
	}

	missing := NewTracker()
	if errLoad := missing.LoadFrom(filepath.Join(t.TempDir(), "missing.json")); errLoad != nil {
		t.Fatalf("LoadFrom(missing) error = %v", errLoad)
	}

	if errWrite := os.WriteFile(path, []byte("not json"), 0o600); errWrite != nil {
		t.Fatalf("write corrupt snapshot: %v", errWrite)
	}
	corrupt := NewTracker()
	corrupt.SetLimits(limits)
	if errLoad := corrupt.LoadFrom(path); errLoad == nil {
		t.Fatal("LoadFrom(corrupt) error = nil, want error")
	}
	if got := corrupt.Snapshot(key, now).RequestsUsed; got != 0 {
		t.Fatalf("corrupt snapshot retained requests = %d, want 0", got)
	}
}

func TestPersistenceDiscardsUnconfiguredKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-limits.json")
	now := time.Date(2026, time.July, 25, 10, 30, 0, 0, time.UTC)

	source := NewTracker()
	source.SetLimits(map[string]Limits{"old": {MaxRequests: 2, Resets: PeriodDaily}})
	source.Allow("old", now)
	if errFlush := source.Flush(path); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}

	restored := NewTracker()
	if errLoad := restored.LoadFrom(path); errLoad != nil {
		t.Fatalf("LoadFrom() error = %v", errLoad)
	}
	restored.SetLimits(map[string]Limits{"new": {MaxRequests: 2, Resets: PeriodDaily}})
	if snapshot := restored.Snapshot("old", now); snapshot != nil {
		t.Fatalf("old key Snapshot() = %+v, want nil", snapshot)
	}
	if errFlush := restored.Flush(path); errFlush != nil {
		t.Fatalf("Flush() after discarding old key error = %v", errFlush)
	}
	contents, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if bytes.Contains(contents, []byte(keyHash("old"))) {
		t.Fatal("discarded key hash remained persisted")
	}
}

func TestAllowConcurrentLimit(t *testing.T) {
	const (
		limit      = int64(500)
		goroutines = 20
		attempts   = 100
	)
	now := time.Date(2026, time.July, 25, 10, 30, 0, 0, time.UTC)
	tracker := NewTracker()
	tracker.SetLimits(map[string]Limits{"key": {MaxRequests: limit, Resets: PeriodHourly}})

	results := make(chan bool, goroutines*attempts)
	var group sync.WaitGroup
	for range goroutines {
		group.Add(1)
		go func() {
			defer group.Done()
			for range attempts {
				results <- tracker.Allow("key", now).Allowed
			}
		}()
	}
	group.Wait()
	close(results)

	var successes int64
	for allowed := range results {
		if allowed {
			successes++
		}
	}
	if successes != limit {
		t.Fatalf("allowed successes = %d, want %d", successes, limit)
	}
}
