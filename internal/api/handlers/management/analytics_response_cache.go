package management

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"golang.org/x/sync/singleflight"
)

const (
	analyticsQueryHeader             = "X-Analytics-Query"
	analyticsResponseCacheTTL        = time.Minute
	analyticsResponseCacheMaxEntries = 128
	analyticsResponseCacheMaxBytes   = 16 << 20
)

type analyticsCachedResponse struct {
	key       string
	body      []byte
	etag      string
	expiresAt time.Time
}

// analyticsResponseCache shares successful management query snapshots across
// clients for one minute. Authentication still runs before every cache lookup.
type analyticsResponseCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     list.List
	bytes   int
	flights singleflight.Group
	now     func() time.Time
}

func (cache *analyticsResponseCache) get(key string) (analyticsCachedResponse, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	element := cache.entries[key]
	if element == nil {
		return analyticsCachedResponse{}, false
	}
	value := element.Value.(analyticsCachedResponse)
	if !cache.now().Before(value.expiresAt) {
		cache.remove(element)
		return analyticsCachedResponse{}, false
	}
	cache.lru.MoveToFront(element)
	return value, true
}

func (cache *analyticsResponseCache) remove(element *list.Element) {
	value := element.Value.(analyticsCachedResponse)
	delete(cache.entries, value.key)
	cache.bytes -= len(value.body)
	cache.lru.Remove(element)
}

func (cache *analyticsResponseCache) do(ctx context.Context, key string, load func() ([]byte, error)) (analyticsCachedResponse, error) {
	if value, ok := cache.get(key); ok {
		return value, nil
	}
	result := cache.flights.DoChan(key, func() (result any, err error) {
		// Singleflight executes outside Gin's recovery middleware.
		defer func() {
			if recover() != nil {
				result, err = nil, cpauk.ErrInternal
			}
		}()
		if value, ok := cache.get(key); ok {
			return value, nil
		}
		body, err := load()
		if err != nil {
			return nil, err
		}
		digest := sha256.New()
		_, _ = digest.Write([]byte(key))
		_, _ = digest.Write(body)
		value := analyticsCachedResponse{
			key: key, body: body, etag: `"` + hex.EncodeToString(digest.Sum(nil)) + `"`,
			expiresAt: cache.now().Add(analyticsResponseCacheTTL),
		}
		if len(body) > analyticsResponseCacheMaxBytes {
			return value, nil
		}
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if cache.entries == nil {
			cache.entries = make(map[string]*list.Element)
		}
		if previous := cache.entries[key]; previous != nil {
			cache.remove(previous)
		}
		cache.entries[key] = cache.lru.PushFront(value)
		cache.bytes += len(body)
		for len(cache.entries) > analyticsResponseCacheMaxEntries || cache.bytes > analyticsResponseCacheMaxBytes {
			cache.remove(cache.lru.Back())
		}
		return value, nil
	})
	select {
	case <-ctx.Done():
		return analyticsCachedResponse{}, ctx.Err()
	case completed := <-result:
		if completed.Err != nil {
			return analyticsCachedResponse{}, completed.Err
		}
		return completed.Val.(analyticsCachedResponse), nil
	}
}

// GetAnalyticsQuery carries query JSON in a bounded header so authenticated
// conditional GETs never put key identities or filters in URLs and access logs.
func (h *Handler) GetAnalyticsQuery(c *gin.Context) {
	headers := c.Request.Header.Values(analyticsQueryHeader)
	if len(headers) != 1 || c.Request.URL.RawQuery != "" || len(headers[0]) > base64.RawURLEncoding.EncodedLen(model.MaxQueryBodyBytes) {
		writeAnalyticsInvalid(c, fmt.Errorf("one bounded query header is required"))
		return
	}
	if c.Request.Body != nil {
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1))
		if err != nil || len(body) != 0 {
			writeAnalyticsInvalid(c, fmt.Errorf("GET query cannot contain a body"))
			return
		}
	}
	data, err := base64.RawURLEncoding.DecodeString(headers[0])
	if err != nil || len(data) == 0 || len(data) > model.MaxQueryBodyBytes {
		writeAnalyticsInvalid(c, fmt.Errorf("invalid query header"))
		return
	}
	h.postAnalyticsQueryData(c, data)
}

func (h *Handler) executeAnalyticsQuery(c *gin.Context, query, selection model.Query) {
	if h == nil {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	h.mu.Lock()
	service := h.analytics
	if h.analyticsResponseCache == nil {
		h.analyticsResponseCache = &analyticsResponseCache{now: time.Now}
	}
	cache := h.analyticsResponseCache
	h.mu.Unlock()
	if service == nil {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	// Legacy and public viewer routes retain their existing response contract.
	if !strings.HasPrefix(c.Request.URL.Path, "/v8/management/analytics/") {
		result, err := h.readAnalyticsQuery(c.Request.Context(), service, query)
		if err != nil {
			writeAnalyticsError(c, classifyAnalyticsReadError(err))
			return
		}
		setAnalyticsNoStore(c)
		c.JSON(http.StatusOK, result)
		return
	}
	epoch := ""
	if scoped, ok := service.(interface{ AnalyticsResponseCacheEpoch() (string, error) }); ok {
		var err error
		epoch, err = scoped.AnalyticsResponseCacheEpoch()
		if err != nil {
			writeAnalyticsError(c, classifyAnalyticsReadError(err))
			return
		}
	} else {
		capabilities := service.Capabilities()
		if !capabilities.Enabled {
			writeAnalyticsError(c, cpauk.ErrDisabled)
			return
		}
		if !capabilities.Available {
			writeAnalyticsError(c, cpauk.ErrUnavailable)
			return
		}
	}
	selection.KeyIDs = slices.Clone(selection.KeyIDs)
	slices.Sort(selection.KeyIDs)
	selection.Start, selection.End = selection.Start.UTC(), selection.End.UTC()
	encoded, err := json.Marshal(struct {
		Epoch string      `json:"epoch"`
		Query model.Query `json:"query"`
	}{Epoch: epoch, Query: quantizeAnalyticsCacheQuery(selection)})
	if err != nil {
		writeAnalyticsError(c, err)
		return
	}
	digest := sha256.Sum256(encoded)
	requestContext := c.Request.Context()
	response, err := cache.do(requestContext, hex.EncodeToString(digest[:]), func() ([]byte, error) {
		if selection.Range != nil {
			query = selection
			if err := resolveAnalyticsRange(&query, time.Now().UTC()); err != nil {
				return nil, err
			}
		}
		result, err := h.readAnalyticsQuery(requestContext, service, query)
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	})
	if err != nil {
		writeAnalyticsError(c, classifyAnalyticsReadError(err))
		return
	}
	h.mu.Lock()
	currentCache := h.analyticsResponseCache
	h.mu.Unlock()
	if currentCache != cache {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	if scoped, ok := service.(interface{ AnalyticsResponseCacheEpoch() (string, error) }); ok {
		current, err := scoped.AnalyticsResponseCacheEpoch()
		if err != nil {
			writeAnalyticsError(c, classifyAnalyticsReadError(err))
			return
		}
		if current != epoch {
			writeAnalyticsError(c, cpauk.ErrUnavailable)
			return
		}
	}
	setAnalyticsNoStore(c)
	c.Header("ETag", response.etag)
	c.Writer.Header().Add("Vary", analyticsQueryHeader+", Authorization, X-Management-Key")
	if c.Request.Method == http.MethodGet && analyticsETagMatches(strings.Join(c.Request.Header.Values("If-None-Match"), ","), response.etag) {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", response.body)
}

// quantizeAnalyticsCacheQuery groups first-page explicit and custom ranges by
// UTC minute. Computation and response metadata retain the first request's
// exact range. Cursor continuations keep exact bounds because their signatures
// are bound to the range returned by that first page.
func quantizeAnalyticsCacheQuery(query model.Query) model.Query {
	if query.Cursor != "" {
		return query
	}
	query.Start = query.Start.UTC().Truncate(time.Minute)
	query.End = query.End.UTC().Truncate(time.Minute)
	if query.Range != nil && query.Range.Preset == "custom" {
		rangeCopy := *query.Range
		rangeCopy.Start = rangeCopy.Start.UTC().Truncate(time.Minute)
		rangeCopy.End = rangeCopy.End.UTC().Truncate(time.Minute)
		query.Range = &rangeCopy
	}
	return query
}

func analyticsETagMatches(condition, etag string) bool {
	for _, candidate := range strings.Split(condition, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}
