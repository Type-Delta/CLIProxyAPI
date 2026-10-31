package chstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// The journal is bounded independently of the result LRU. Overflow starts a new
// generation rather than keeping an unbounded map of every historical bucket.
const historyMaxMutations = 256

type historyMutation struct {
	start, end time.Time
	revision   uint64
}

func (s *Store) now() time.Time {
	if s.nowFunc != nil {
		return s.nowFunc().UTC()
	}
	return time.Now().UTC()
}

// Call under mu. Invalidate after visible publication, including partial writes
// on error, before releasing the writer lock. A broad mutation drops the journal.
func (s *Store) clearHistory() {
	s.historyRevision++
	s.historyGeneration = s.historyRevision
	s.historyMutations = nil
	s.historyCache.Clear()
}

func (s *Store) invalidateHistory(start, end time.Time) {
	// Coalesce mutations in the same 15-minute interval so normal tail intake
	// does not exhaust the journal on every batch. Ranges remain half-open.
	start = start.UTC().Truncate(15 * time.Minute)
	end = end.UTC().Add(-time.Nanosecond).Truncate(15 * time.Minute).Add(15 * time.Minute)
	for index, mutation := range s.historyMutations {
		if mutation.start.Equal(start) && mutation.end.Equal(end) {
			s.historyRevision++
			s.historyMutations[index].revision = s.historyRevision
			return
		}
	}
	if len(s.historyMutations) == historyMaxMutations {
		s.clearHistory()
	}
	s.historyRevision++
	s.historyMutations = append(s.historyMutations, historyMutation{start, end, s.historyRevision})
}

func (s *Store) invalidateEventHistory(events []model.Event) {
	if len(events) == 0 {
		return
	}
	start, end := events[0].RequestedAt, events[0].RequestedAt
	for _, event := range events {
		if event.RequestedAt.Before(start) {
			start = event.RequestedAt
		}
		if event.RequestedAt.After(end) {
			end = event.RequestedAt
		}
	}
	s.invalidateHistory(start, end.Add(time.Nanosecond))
}

// Native batches and INSERT SELECT must be visible before invalidating and
// unlocking, even if the connection's DSN enables asynchronous inserts.
// Submitted write calls also detach cancellation until acknowledgement; a server
// write can otherwise become visible after the writer has released mu.
func synchronousWriteContext(ctx context.Context) context.Context {
	return ch.Context(context.WithoutCancel(ctx), ch.WithSettings(ch.Settings{"async_insert": 0, "wait_for_async_insert": 1}))
}

func (s *Store) historyKey(q model.Query) (string, bool) {
	// Today's ranges stay live, including ranges whose end is already in the past.
	today, err := activityDayStart(s.now(), q.TimeZone)
	if err != nil || s.historyImporting || s.historyPending || q.End.IsZero() || q.End.After(today) {
		return "", false
	}
	selection, err := backend.HistoryQueryDigest(q)
	if err != nil {
		return "", false
	}
	generation := s.historyGeneration
	for _, mutation := range s.historyMutations {
		if mutation.start.Before(q.End) && mutation.end.After(q.Start) {
			generation = max(generation, mutation.revision)
		}
	}
	data, err := json.Marshal(struct {
		Selection, Identity string
		Generation          uint64
	}{selection, s.identityEpoch, generation})
	if err != nil {
		return "", false
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), true
}

// Computation may take several read locks. Validate the mutation revision before
// publishing so a concurrent mutation cannot cache a mixed or obsolete result.
func cachedHistory(s *Store, ctx context.Context, q model.Query, read func() (backend.HistoryCacheValue, error)) (backend.HistoryCacheValue, error) {
	if err := ctx.Err(); err != nil {
		return backend.HistoryCacheValue{}, err
	}
	s.mu.RLock()
	if s.db == nil {
		s.mu.RUnlock()
		return backend.HistoryCacheValue{}, ErrClosed
	}
	key, cacheable := s.historyKey(q)
	revision := s.historyRevision
	if cacheable {
		if value, ok := s.historyCache.Get(key); ok {
			s.mu.RUnlock()
			return value, nil
		}
	}
	s.mu.RUnlock()
	value, err := read()
	if err != nil {
		return value, err
	}
	if cacheable && (q.Operation != model.OperationAnalysis || backend.AnalysisResultComplete(value.Analysis)) {
		s.mu.RLock()
		if s.db != nil && revision == s.historyRevision {
			s.historyCache.Put(key, value)
		}
		s.mu.RUnlock()
	}
	return value, nil
}
