package backend

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// Historical analytics are read often by dashboards while their selected
// ranges change infrequently. Keep a small process-local cache for those
// reads. Each backend supplies mutation-aware keys for its persisted history.
// A process restart begins with an empty cache.
const (
	HistoryCacheMaxEntries = 128
	HistoryCacheMaxBytes   = 32 << 20
)

type HistoryCacheValue struct {
	Operation  model.Operation
	Summary    model.Summary
	Timeseries model.Timeseries
	Activity   model.Activity
	Analysis   model.Analysis
}

type historyCacheEntry struct {
	key   string
	value HistoryCacheValue
	size  int64
}

type HistoryResultCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List
	bytes   int64
}

func (c *HistoryResultCache) initializeLocked() {
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
		c.lru = list.New()
	}
}

func (c *HistoryResultCache) Get(key string) (HistoryCacheValue, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initializeLocked()
	element, ok := c.entries[key]
	if !ok {
		return HistoryCacheValue{}, false
	}
	c.lru.MoveToFront(element)
	entry := element.Value.(*historyCacheEntry)
	return cloneHistoryCacheValue(entry.value), true
}

func (c *HistoryResultCache) Put(key string, value HistoryCacheValue) {
	encoded, err := marshalHistoryCacheValue(value)
	if err != nil {
		return
	}
	size := int64(len(encoded) + len(key))
	if size > HistoryCacheMaxBytes {
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
	for len(c.entries) > HistoryCacheMaxEntries || c.bytes > HistoryCacheMaxBytes {
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

func marshalHistoryCacheValue(value HistoryCacheValue) ([]byte, error) {
	switch value.Operation {
	case model.OperationSummary:
		return json.Marshal(value.Summary)
	case model.OperationTimeseries:
		return json.Marshal(value.Timeseries)
	case model.OperationActivity:
		return json.Marshal(value.Activity)
	case model.OperationAnalysis:
		return json.Marshal(value.Analysis)
	default:
		return nil, nil
	}
}

func (c *HistoryResultCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
	c.lru = nil
	c.bytes = 0
}

// Stats is intentionally package-private so tests and benchmarks
// can verify the memory bound without making cache policy part of the service
// API.
func (c *HistoryResultCache) Stats() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries), c.bytes
}

func cloneHistoryCacheValue(value HistoryCacheValue) HistoryCacheValue {
	result := HistoryCacheValue{Operation: value.Operation}
	switch value.Operation {
	case model.OperationSummary:
		result.Summary = cloneJSON(value.Summary)
	case model.OperationTimeseries:
		result.Timeseries = cloneJSON(value.Timeseries)
	case model.OperationActivity:
		result.Activity = cloneJSON(value.Activity)
	case model.OperationAnalysis:
		result.Analysis = cloneJSON(value.Analysis)
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

// HistoryQueryDigest includes fields that SelectionDigest deliberately omits
// for cursor pagination but which remain part of the exact analytics query
// contract. Named ranges normally have been resolved by the service before a
// store query arrives; retaining the field here avoids accidental collisions
// for direct store callers.
func HistoryQueryDigest(query model.Query) (string, error) {
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

func AnalysisResultComplete(result model.Analysis) bool {
	return result.SeriesByCategory != nil && !result.SeriesByCategory.Meta.Partial &&
		result.ModelByTime != nil && !result.ModelByTime.Meta.Partial &&
		result.Latency != nil && !result.Latency.Meta.Partial &&
		result.CostComponents != nil && !result.CostComponents.Meta.Partial &&
		result.KeyModelMatrix != nil && !result.KeyModelMatrix.Meta.Partial
}
