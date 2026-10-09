package store

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// Historical analytics are read often by dashboards while their selected
// ranges change infrequently. Keep a small process-local cache for those
// reads. The durable history generation digest in the key makes a cache entry
// local to the exact set of persisted history it covers.
const (
	historyCacheMaxEntries = 128
	historyCacheMaxBytes   = 32 << 20
)

type historyCacheValue struct {
	operation  model.Operation
	summary    model.Summary
	timeseries model.Timeseries
	analysis   model.Analysis
}

type historyCacheEntry struct {
	key   string
	value historyCacheValue
	size  int64
}

type historyResultCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List
	bytes   int64
}

func (c *historyResultCache) initializeLocked() {
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
		c.lru = list.New()
	}
}

func (c *historyResultCache) get(key string) (historyCacheValue, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initializeLocked()
	element, ok := c.entries[key]
	if !ok {
		return historyCacheValue{}, false
	}
	c.lru.MoveToFront(element)
	entry := element.Value.(*historyCacheEntry)
	return cloneHistoryCacheValue(entry.value), true
}

func (c *historyResultCache) put(key string, value historyCacheValue) {
	encoded, err := marshalHistoryCacheValue(value)
	if err != nil {
		return
	}
	size := int64(len(encoded) + len(key))
	if size > historyCacheMaxBytes {
		return
	}
	entry := &historyCacheEntry{key: key, value: cloneHistoryCacheValue(value), size: size}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initializeLocked()
	if old, ok := c.entries[key]; ok {
		c.bytes -= old.Value.(*historyCacheEntry).size
		c.lru.Remove(old)
		delete(c.entries, key)
	}
	element := c.lru.PushFront(entry)
	c.entries[key] = element
	c.bytes += size
	for len(c.entries) > historyCacheMaxEntries || c.bytes > historyCacheMaxBytes {
		old := c.lru.Back()
		if old == nil {
			break
		}
		c.lru.Remove(old)
		item := old.Value.(*historyCacheEntry)
		delete(c.entries, item.key)
		c.bytes -= item.size
	}
}

func marshalHistoryCacheValue(value historyCacheValue) ([]byte, error) {
	switch value.operation {
	case model.OperationSummary:
		return json.Marshal(value.summary)
	case model.OperationTimeseries:
		return json.Marshal(value.timeseries)
	case model.OperationAnalysis:
		return json.Marshal(value.analysis)
	default:
		return nil, nil
	}
}

func (c *historyResultCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
	c.lru = nil
	c.bytes = 0
}

// historyCacheStats is intentionally package-private so tests and benchmarks
// can verify the memory bound without making cache policy part of the service
// API.
func (c *historyResultCache) historyCacheStats() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries), c.bytes
}

func cloneHistoryCacheValue(value historyCacheValue) historyCacheValue {
	result := historyCacheValue{operation: value.operation}
	switch value.operation {
	case model.OperationSummary:
		result.summary = cloneJSON(value.summary)
	case model.OperationTimeseries:
		result.timeseries = cloneJSON(value.timeseries)
	case model.OperationAnalysis:
		result.analysis = cloneJSON(value.analysis)
	}
	return result
}

// cloneJSON is used only for cache ownership boundaries. All cached model
// values are JSON contracts, so this also keeps nested maps and optional
// pointers independent of callers without duplicating every response shape.
func cloneJSON[T any](value T) T {
	data, err := json.Marshal(value)
	if err != nil {
		var zero T
		return zero
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		var zero T
		return zero
	}
	return result
}

func historyQueryIsHistorical(query model.Query) bool {
	return !query.End.IsZero() && !query.End.After(time.Now().UTC())
}

// historyQueryDigest includes fields that SelectionDigest deliberately omits
// for cursor pagination but which remain part of the exact analytics query
// contract. Named ranges normally have been resolved by the service before a
// store query arrives; retaining the field here avoids accidental collisions
// for direct store callers.
func historyQueryDigest(query model.Query) (string, error) {
	selection, err := query.SelectionDigest()
	if err != nil {
		return "", err
	}
	payload := struct {
		Selection string              `json:"selection"`
		Window    string              `json:"window"`
		Range     *model.RangeRequest `json:"range,omitempty"`
	}{Selection: selection, Window: query.Window, Range: query.Range}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// historyPricingDigestLocked includes both current rules and their persisted
// provenance because analysis cost components are checked against the current
// pricing book. The caller holds s.mu.RLock.
func (s *SQLiteStore) historyPricingDigestLocked(ctx context.Context) (string, error) {
	book, provenance, err := loadPricingRules(ctx, s.db)
	if err != nil {
		return "", err
	}
	payload := struct {
		Rules      any               `json:"rules"`
		Provenance PricingProvenance `json:"provenance"`
	}{Rules: book.Rules, Provenance: provenance}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// historyCacheKeyLocked reads the durable range generation while the store
// read lock is held. A caller that publishes a miss must hold that same lock
// while recomputing this key, so a mutation cannot commit between validation
// and insertion.
func (s *SQLiteStore) historyCacheKeyLocked(ctx context.Context, query model.Query) (string, error) {
	selection, err := historyQueryDigest(query)
	if err != nil {
		return "", err
	}
	generation, err := s.historyGenerationLocked(ctx, query.Start.UnixNano(), query.End.UnixNano())
	if err != nil {
		return "", err
	}
	pricing := ""
	if query.Operation == model.OperationAnalysis {
		pricing, err = s.historyPricingDigestLocked(ctx)
		if err != nil {
			return "", err
		}
	}
	payload := struct {
		IdentityEpoch string `json:"identity_epoch"`
		Selection     string `json:"selection"`
		Generation    string `json:"generation"`
		Pricing       string `json:"pricing,omitempty"`
	}{IdentityEpoch: s.identityEpoch, Selection: selection, Generation: generation, Pricing: pricing}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// historyCacheGet prepares a key and performs the lookup while holding the
// store read lock. Keeping the lock across both operations gives a hit a
// snapshot boundary: a mutation cannot commit after the generation was read
// and before the cached value is returned.
func (s *SQLiteStore) historyCacheGet(ctx context.Context, query model.Query, validateRetained bool) (string, bool, historyCacheValue, bool, error) {
	if !historyQueryIsHistorical(query) {
		return "", false, historyCacheValue{}, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return "", false, historyCacheValue{}, false, ErrClosed
	}
	if validateRetained {
		if err := s.validateRetainedRange(ctx, query); err != nil {
			return "", false, historyCacheValue{}, false, err
		}
	}
	key, err := s.historyCacheKeyLocked(ctx, query)
	if err != nil {
		// Cache infrastructure failures must not change analytics behavior. The
		// uncached path below remains authoritative.
		return "", false, historyCacheValue{}, false, nil
	}
	value, ok := s.historyCache.get(key)
	return key, true, value, ok, nil
}

func (s *SQLiteStore) publishHistoryCache(ctx context.Context, query model.Query, key string, value historyCacheValue) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return
	}
	current, err := s.historyCacheKeyLocked(ctx, query)
	if err == nil && current == key {
		s.historyCache.put(key, value)
	}
}

func (s *SQLiteStore) cachedSummary(ctx context.Context, query model.Query) (model.Summary, error) {
	key, cacheable, value, hit, err := s.historyCacheGet(ctx, query, true)
	if err != nil {
		return model.Summary{}, err
	}
	if cacheable && hit {
		return value.summary, nil
	}
	result, err := s.summaryUncached(ctx, query)
	if err != nil {
		return model.Summary{}, err
	}
	if cacheable {
		s.publishHistoryCache(ctx, query, key, historyCacheValue{operation: model.OperationSummary, summary: result})
	}
	return result, nil
}

func (s *SQLiteStore) cachedTimeseries(ctx context.Context, query model.Query) (model.Timeseries, error) {
	key, cacheable, value, hit, err := s.historyCacheGet(ctx, query, true)
	if err != nil {
		return model.Timeseries{}, err
	}
	if cacheable && hit {
		return value.timeseries, nil
	}
	result, err := s.timeseriesUncached(ctx, query)
	if err != nil {
		return model.Timeseries{}, err
	}
	if cacheable {
		s.publishHistoryCache(ctx, query, key, historyCacheValue{operation: model.OperationTimeseries, timeseries: result})
	}
	return result, nil
}

func analysisResultComplete(result model.Analysis) bool {
	return result.SeriesByCategory != nil && !result.SeriesByCategory.Meta.Partial &&
		result.ModelByTime != nil && !result.ModelByTime.Meta.Partial &&
		result.Latency != nil && !result.Latency.Meta.Partial &&
		result.CostComponents != nil && !result.CostComponents.Meta.Partial &&
		result.KeyModelMatrix != nil && !result.KeyModelMatrix.Meta.Partial
}

func (s *SQLiteStore) cachedAnalysis(ctx context.Context, query model.Query) (model.Analysis, error) {
	key, cacheable, value, hit, err := s.historyCacheGet(ctx, query, false)
	if err != nil {
		return model.Analysis{}, err
	}
	if cacheable && hit {
		return value.analysis, nil
	}
	result, err := s.analysisUncached(ctx, query)
	if err != nil {
		return model.Analysis{}, err
	}
	if cacheable && analysisResultComplete(result) {
		s.publishHistoryCache(ctx, query, key, historyCacheValue{operation: model.OperationAnalysis, analysis: result})
	}
	return result, nil
}
