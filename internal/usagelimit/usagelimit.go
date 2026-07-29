// Package usagelimit tracks per-key request and token usage.
package usagelimit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

const maxInt64 = int64(^uint64(0) >> 1)

// Period identifies the reset cadence for a key's limits.
type Period string

const (
	PeriodLifetime Period = ""
	PeriodHourly   Period = "hourly"
	PeriodDaily    Period = "daily"
	PeriodWeekly   Period = "weekly"
	PeriodMonthly  Period = "monthly"
)

// ParsePeriod normalizes a configured value. Empty or whitespace is lifetime.
func ParsePeriod(value string) (Period, error) {
	switch strings.TrimSpace(value) {
	case "":
		return PeriodLifetime, nil
	case string(PeriodHourly):
		return PeriodHourly, nil
	case string(PeriodDaily):
		return PeriodDaily, nil
	case string(PeriodWeekly):
		return PeriodWeekly, nil
	case string(PeriodMonthly):
		return PeriodMonthly, nil
	default:
		return PeriodLifetime, fmt.Errorf("unknown usage limit reset period %q", value)
	}
}

// Limits holds the caps for one API key. Zero means unlimited for that metric.
type Limits struct {
	MaxRequests int64
	MaxTokens   int64
	Resets      Period
}

// IsZero reports whether neither metric is capped.
func (l Limits) IsZero() bool {
	return l.MaxRequests == 0 && l.MaxTokens == 0
}

// Snapshot reports current consumption for one key.
type Snapshot struct {
	MaxRequests  int64      `json:"max_requests"`
	RequestsUsed int64      `json:"requests_used"`
	MaxTokens    int64      `json:"max_tokens"`
	TokensUsed   int64      `json:"tokens_used"`
	Resets       string     `json:"resets"`
	ResetAt      *time.Time `json:"reset_at,omitempty"`
}

// Decision is the outcome of a pre-flight gate check.
type Decision struct {
	Allowed bool
	Metric  string
	Limit   int64
	Used    int64
	Resets  Period
	ResetAt *time.Time
}

type counter struct {
	windowID int64
	requests int64
	tokens   int64
	// resets records the cadence that produced windowID. Window ids from
	// different cadences are not comparable, so the cadence must travel with
	// the counter, including across a restart.
	resets      Period
	initialized bool
}

type persistedFile struct {
	Version int              `json:"version"`
	SavedAt time.Time        `json:"saved_at"`
	Entries []persistedEntry `json:"entries"`
}

type persistedEntry struct {
	KeyHash string `json:"key_hash"`
	// Resets records the cadence the window id was computed under, so a counter
	// restored after a cadence change is not compared against an incompatible
	// numbering space.
	Resets   string `json:"resets,omitempty"`
	WindowID int64  `json:"window_id"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// Tracker holds per-key counters. It is safe for concurrent use.
type Tracker struct {
	mu        sync.Mutex
	limits    map[string]Limits
	counters  map[string]*counter
	persisted map[string]counter
	dirty     bool
}

// NewTracker creates an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{
		limits:    make(map[string]Limits),
		counters:  make(map[string]*counter),
		persisted: make(map[string]counter),
		dirty:     true,
	}
}

// SetLimits replaces the limit table while preserving counters for surviving
// keys. A reset cadence change clears that key's counters.
func (t *Tracker) SetLimits(limits map[string]Limits) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.setLimitsLocked(limits)
}

func (t *Tracker) setLimitsLocked(limits map[string]Limits) {
	updated := make(map[string]Limits, len(limits))
	for key, limit := range limits {
		updated[key] = limit
	}

	for key := range t.counters {
		newLimit, exists := updated[key]
		if !exists {
			delete(t.counters, key)
			t.dirty = true
			continue
		}
		if t.limits[key].Resets != newLimit.Resets {
			delete(t.counters, key)
			t.dirty = true
			continue
		}
	}

	for key := range updated {
		hash := keyHash(key)
		persisted, exists := t.persisted[hash]
		if !exists {
			continue
		}
		if _, alreadyTracked := t.counters[key]; !alreadyTracked {
			persisted.initialized = true
			t.counters[key] = &persisted
		}
		delete(t.persisted, hash)
	}
	if len(t.persisted) > 0 {
		t.dirty = true
		t.persisted = make(map[string]counter)
	}

	t.limits = updated
}

// Allow checks whether a request may proceed and records an allowed request.
func (t *Tracker) Allow(key string, now time.Time) Decision {
	t.mu.Lock()
	defer t.mu.Unlock()

	limits, exists := t.limits[key]
	if !exists || limits.IsZero() {
		return Decision{Allowed: true}
	}

	counters := t.counterFor(key)
	resetAt, changed := counters.advance(now, limits.Resets)
	if changed {
		t.dirty = true
	}

	if limits.MaxRequests > 0 && counters.requests >= limits.MaxRequests {
		return deniedDecision("requests", limits.MaxRequests, counters.requests, limits.Resets, resetAt)
	}
	if limits.MaxTokens > 0 && counters.tokens >= limits.MaxTokens {
		return deniedDecision("tokens", limits.MaxTokens, counters.tokens, limits.Resets, resetAt)
	}

	if counters.requests < maxInt64 {
		counters.requests++
		t.dirty = true
	}
	return Decision{Allowed: true}
}

// AddTokens records observed token usage for a key.
func (t *Tracker) AddTokens(key string, tokens int64, now time.Time) {
	if tokens <= 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	limits, exists := t.limits[key]
	if !exists || limits.IsZero() {
		return
	}

	counters := t.counterFor(key)
	_, changed := counters.advance(now, limits.Resets)
	if counters.tokens > maxInt64-tokens {
		counters.tokens = maxInt64
		changed = true
	} else {
		counters.tokens += tokens
		changed = true
	}
	if changed {
		t.dirty = true
	}
}

// Reset clears the current usage for a configured, limited key.
func (t *Tracker) Reset(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	limits, exists := t.limits[key]
	if !exists || limits.IsZero() {
		return false
	}
	delete(t.counters, key)
	delete(t.persisted, keyHash(key))
	t.dirty = true
	return true
}

// Snapshot returns current consumption for key, or nil when it is unlimited or
// not configured.
func (t *Tracker) Snapshot(key string, now time.Time) *Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()

	limits, exists := t.limits[key]
	if !exists || limits.IsZero() {
		return nil
	}

	counters := t.counterFor(key)
	resetAt, changed := counters.advance(now, limits.Resets)
	if changed {
		t.dirty = true
	}
	return &Snapshot{
		MaxRequests:  limits.MaxRequests,
		RequestsUsed: counters.requests,
		MaxTokens:    limits.MaxTokens,
		TokensUsed:   counters.tokens,
		Resets:       resetName(limits.Resets),
		ResetAt:      resetAt,
	}
}

// Keys returns configured keys in sorted order.
func (t *Tracker) Keys() []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	keys := make([]string, 0, len(t.limits))
	for key := range t.limits {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// LoadFrom restores persisted counters from path. Missing files are ignored.
func (t *Tracker) LoadFrom(path string) error {
	contents, errRead := os.ReadFile(path)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			t.replacePersisted(nil, false)
			return nil
		}
		return t.loadError(path, fmt.Errorf("read usage limit snapshot: %w", errRead))
	}

	var stored persistedFile
	if errUnmarshal := json.Unmarshal(contents, &stored); errUnmarshal != nil {
		return t.loadError(path, fmt.Errorf("decode usage limit snapshot: %w", errUnmarshal))
	}
	if stored.Version != 1 {
		return t.loadError(path, fmt.Errorf("unsupported usage limit snapshot version %d", stored.Version))
	}

	entries := make(map[string]counter, len(stored.Entries))
	for _, entry := range stored.Entries {
		hash, errHash := validKeyHash(entry.KeyHash)
		if errHash != nil {
			return t.loadError(path, fmt.Errorf("invalid usage limit snapshot entry: %w", errHash))
		}
		if entry.Requests < 0 || entry.Tokens < 0 {
			return t.loadError(path, fmt.Errorf("invalid usage limit snapshot counter"))
		}
		if _, duplicate := entries[hash]; duplicate {
			return t.loadError(path, fmt.Errorf("duplicate usage limit snapshot entry"))
		}
		resets, errPeriod := ParsePeriod(entry.Resets)
		if errPeriod != nil {
			return t.loadError(path, fmt.Errorf("invalid usage limit snapshot entry: %w", errPeriod))
		}
		// A window id ahead of the current one cannot be produced legitimately.
		// Because window ids only move forward, adopting one would freeze the
		// counter and report an absurd reset time, so drop the entry and let the
		// key start clean.
		if currentWindow, _ := periodWindow(time.Now(), resets); entry.WindowID > currentWindow {
			logrus.WithField("path", path).Warn("discard usage limit snapshot entry with a future window")
			continue
		}
		entries[hash] = counter{
			windowID:    entry.WindowID,
			requests:    entry.Requests,
			tokens:      entry.Tokens,
			resets:      resets,
			initialized: true,
		}
	}

	t.replacePersisted(entries, false)
	return nil
}

// Flush atomically writes persisted counters to path.
func (t *Tracker) Flush(path string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.dirty {
		return nil
	}

	entries := t.entriesForFlush()
	contents, errMarshal := json.Marshal(persistedFile{
		Version: 1,
		SavedAt: time.Now().UTC(),
		Entries: entries,
	})
	if errMarshal != nil {
		return fmt.Errorf("encode usage limit snapshot: %w", errMarshal)
	}

	directory := filepath.Dir(path)
	if errMkdir := os.MkdirAll(directory, 0o700); errMkdir != nil {
		return fmt.Errorf("create usage limit snapshot directory: %w", errMkdir)
	}
	temporary, errCreate := os.CreateTemp(directory, filepath.Base(path)+".tmp-*")
	if errCreate != nil {
		return fmt.Errorf("create usage limit snapshot: %w", errCreate)
	}
	temporaryName := temporary.Name()
	defer func() {
		if errRemove := os.Remove(temporaryName); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			logrus.WithError(errRemove).Warn("remove temporary usage limit snapshot")
		}
	}()

	if errChmod := temporary.Chmod(0o600); errChmod != nil {
		_ = temporary.Close()
		return fmt.Errorf("set usage limit snapshot permissions: %w", errChmod)
	}
	if _, errWrite := temporary.Write(contents); errWrite != nil {
		_ = temporary.Close()
		return fmt.Errorf("write usage limit snapshot: %w", errWrite)
	}
	if errClose := temporary.Close(); errClose != nil {
		return fmt.Errorf("close usage limit snapshot: %w", errClose)
	}
	if errRename := os.Rename(temporaryName, path); errRename != nil {
		return fmt.Errorf("replace usage limit snapshot: %w", errRename)
	}

	t.dirty = false
	return nil
}

func (t *Tracker) counterFor(key string) *counter {
	counters := t.counters[key]
	if counters == nil {
		counters = &counter{}
		t.counters[key] = counters
	}
	return counters
}

func (t *Tracker) replacePersisted(entries map[string]counter, dirty bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.counters = make(map[string]*counter)
	t.persisted = make(map[string]counter, len(entries))
	for hash, entry := range entries {
		t.persisted[hash] = entry
	}
	t.dirty = dirty
	if len(t.limits) > 0 {
		t.setLimitsLocked(t.limits)
	}
}

func (t *Tracker) loadError(path string, errLoad error) error {
	logrus.WithError(errLoad).WithField("path", path).Warn("ignore usage limit snapshot")
	t.replacePersisted(nil, false)
	return errLoad
}

func (t *Tracker) entriesForFlush() []persistedEntry {
	byHash := make(map[string]counter, len(t.counters)+len(t.persisted))
	for key, counters := range t.counters {
		if counters.initialized {
			byHash[keyHash(key)] = *counters
		}
	}
	for hash, counters := range t.persisted {
		byHash[hash] = counters
	}

	hashes := make([]string, 0, len(byHash))
	for hash := range byHash {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)

	entries := make([]persistedEntry, 0, len(hashes))
	for _, hash := range hashes {
		counters := byHash[hash]
		entries = append(entries, persistedEntry{
			KeyHash:  hash,
			Resets:   string(counters.resets),
			WindowID: counters.windowID,
			Requests: counters.requests,
			Tokens:   counters.tokens,
		})
	}
	return entries
}

func (c *counter) advance(now time.Time, period Period) (*time.Time, bool) {
	windowID, resetAt := periodWindow(now, period)
	if !c.initialized {
		c.windowID = windowID
		c.resets = period
		c.initialized = true
		return resetAt, true
	}
	if c.resets != period {
		// The cadence changed (config edit, or a counter restored from disk
		// that was written under a different cadence). Window ids live in
		// unrelated numbering spaces, so comparing them would either freeze the
		// window forever or reset it spuriously. Start the new cadence clean.
		c.windowID = windowID
		c.resets = period
		c.requests = 0
		c.tokens = 0
		return resetAt, true
	}
	if windowID > c.windowID {
		c.windowID = windowID
		c.requests = 0
		c.tokens = 0
		return resetAt, true
	}
	if windowID < c.windowID {
		return resetAtForWindow(c.windowID, period), false
	}
	return resetAt, false
}

func deniedDecision(metric string, limit, used int64, resets Period, resetAt *time.Time) Decision {
	return Decision{
		Allowed: false,
		Metric:  metric,
		Limit:   limit,
		Used:    used,
		Resets:  resets,
		ResetAt: resetAt,
	}
}

func periodWindow(now time.Time, period Period) (int64, *time.Time) {
	utc := now.UTC()
	switch period {
	case PeriodHourly:
		windowID := utc.Unix() / 3600
		resetAt := utc.Truncate(time.Hour).Add(time.Hour)
		return windowID, &resetAt
	case PeriodDaily:
		year, month, day := utc.Date()
		windowID := utc.Unix() / 86400
		resetAt := time.Date(year, month, day+1, 0, 0, 0, 0, time.UTC)
		return windowID, &resetAt
	case PeriodWeekly:
		isoYear, isoWeek := utc.ISOWeek()
		windowID := int64(isoYear*100 + isoWeek)
		weekdayOffset := (int(utc.Weekday()) + 6) % 7
		monday := time.Date(utc.Year(), utc.Month(), utc.Day()-weekdayOffset, 0, 0, 0, 0, time.UTC)
		resetAt := monday.AddDate(0, 0, 7)
		return windowID, &resetAt
	case PeriodMonthly:
		year, month, _ := utc.Date()
		windowID := int64(year*12 + int(month))
		resetAt := time.Date(year, month+1, 1, 0, 0, 0, 0, time.UTC)
		return windowID, &resetAt
	default:
		return 0, nil
	}
}

func resetAtForWindow(windowID int64, period Period) *time.Time {
	switch period {
	case PeriodHourly:
		resetAt := time.Unix(windowID*3600, 0).UTC().Add(time.Hour)
		return &resetAt
	case PeriodDaily:
		resetAt := time.Unix(windowID*86400, 0).UTC().AddDate(0, 0, 1)
		return &resetAt
	case PeriodWeekly:
		isoYear := int(windowID / 100)
		isoWeek := int(windowID % 100)
		janFourth := time.Date(isoYear, time.January, 4, 0, 0, 0, 0, time.UTC)
		firstMonday := janFourth.AddDate(0, 0, -(int(janFourth.Weekday())+6)%7)
		resetAt := firstMonday.AddDate(0, 0, isoWeek*7)
		return &resetAt
	case PeriodMonthly:
		year := int(windowID / 12)
		month := time.Month(windowID % 12)
		resetAt := time.Date(year, month+1, 1, 0, 0, 0, 0, time.UTC)
		return &resetAt
	default:
		return nil
	}
}

func resetName(period Period) string {
	if period == PeriodLifetime {
		return "lifetime"
	}
	return string(period)
}

func keyHash(key string) string {
	hash := sha256.Sum256([]byte(key))
	return hex.EncodeToString(hash[:])
}

func validKeyHash(value string) (string, error) {
	if len(value) != sha256.Size*2 {
		return "", fmt.Errorf("key hash has length %d, want %d", len(value), sha256.Size*2)
	}
	if _, errDecode := hex.DecodeString(value); errDecode != nil {
		return "", fmt.Errorf("key hash is not hexadecimal: %w", errDecode)
	}
	return strings.ToLower(value), nil
}
