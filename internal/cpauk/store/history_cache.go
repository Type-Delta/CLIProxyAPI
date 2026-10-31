package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// Both analytics backends share the bounded LRU and JSON ownership boundaries.
type historyCacheValue = backend.HistoryCacheValue
type historyResultCache struct{ backend.HistoryResultCache }

func (c *historyResultCache) get(key string) (historyCacheValue, bool) { return c.Get(key) }
func (c *historyResultCache) put(key string, value historyCacheValue)  { c.Put(key, value) }
func (c *historyResultCache) clear()                                   { c.Clear() }
func (c *historyResultCache) historyCacheStats() (int, int64)          { return c.Stats() }

func historyQueryIsHistorical(query model.Query) bool {
	return !query.End.IsZero() && !query.End.After(time.Now().UTC())
}

func historyQueryDigest(query model.Query) (string, error) { return backend.HistoryQueryDigest(query) }

// historyPricingDigestLocked includes both current rules and their persisted
// provenance because analysis cost components are checked against the current
// pricing book. The caller holds s.mu.RLock.
func (s *SQLiteStore) historyPricingDigestLocked(ctx context.Context) (string, error) {
	book, provenance, err := loadPricingRules(ctx, s.db)
	if err != nil {
		return "", err
	}
	payload := struct {
		Rate       model.NanoUSD     `json:"rate"`
		Rules      any               `json:"rules"`
		Provenance PricingProvenance `json:"provenance"`
	}{Rate: book.EffectiveKiroRate(), Rules: book.Rules, Provenance: provenance}
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
		return value.Summary, nil
	}
	result, err := s.summaryUncached(ctx, query)
	if err != nil {
		return model.Summary{}, err
	}
	if cacheable {
		s.publishHistoryCache(ctx, query, key, historyCacheValue{Operation: model.OperationSummary, Summary: result})
	}
	return result, nil
}

func (s *SQLiteStore) cachedTimeseries(ctx context.Context, query model.Query) (model.Timeseries, error) {
	key, cacheable, value, hit, err := s.historyCacheGet(ctx, query, true)
	if err != nil {
		return model.Timeseries{}, err
	}
	if cacheable && hit {
		return value.Timeseries, nil
	}
	result, err := s.timeseriesUncached(ctx, query)
	if err != nil {
		return model.Timeseries{}, err
	}
	if cacheable {
		s.publishHistoryCache(ctx, query, key, historyCacheValue{Operation: model.OperationTimeseries, Timeseries: result})
	}
	return result, nil
}

func (s *SQLiteStore) cachedActivity(ctx context.Context, query model.Query) (model.Activity, error) {
	key, cacheable, value, hit, err := s.historyCacheGet(ctx, query, false)
	if err != nil {
		return model.Activity{}, err
	}
	if cacheable && hit {
		return value.Activity, nil
	}
	result, err := s.activityUncached(ctx, query)
	if err != nil {
		return model.Activity{}, err
	}
	if cacheable {
		s.publishHistoryCache(ctx, query, key, historyCacheValue{Operation: model.OperationActivity, Activity: result})
	}
	return result, nil
}

func (s *SQLiteStore) cachedAnalysis(ctx context.Context, query model.Query) (model.Analysis, error) {
	key, cacheable, value, hit, err := s.historyCacheGet(ctx, query, false)
	if err != nil {
		return model.Analysis{}, err
	}
	if cacheable && hit {
		return value.Analysis, nil
	}
	result, err := s.analysisUncached(ctx, query)
	if err != nil {
		return model.Analysis{}, err
	}
	if cacheable && backend.AnalysisResultComplete(result) {
		s.publishHistoryCache(ctx, query, key, historyCacheValue{Operation: model.OperationAnalysis, Analysis: result})
	}
	return result, nil
}
