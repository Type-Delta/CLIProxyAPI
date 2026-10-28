package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk"
	store "github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/chstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

type analyticsResponseReader struct {
	*analyticsHandlerReader
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func (reader *analyticsResponseReader) record() error {
	reader.calls.Add(1)
	if reader.entered != nil {
		reader.entered <- struct{}{}
		<-reader.release
	}
	return reader.err
}

func (reader *analyticsResponseReader) Summary(context.Context, model.Query) (model.Summary, error) {
	err := reader.record()
	return model.Summary{UpstreamAttempts: reader.calls.Load()}, err
}

func (reader *analyticsResponseReader) Timeseries(context.Context, model.Query) (model.Timeseries, error) {
	return model.Timeseries{}, reader.record()
}

func (reader *analyticsResponseReader) Dimensions(context.Context, model.Query) (model.DimensionPage, error) {
	return model.DimensionPage{}, reader.record()
}

func (reader *analyticsResponseReader) Events(context.Context, model.Query) (model.EventPage, error) {
	return model.EventPage{}, reader.record()
}

func (reader *analyticsResponseReader) Leaderboard(context.Context, model.Query) (model.LeaderboardPage, error) {
	return model.LeaderboardPage{}, reader.record()
}

func (reader *analyticsResponseReader) Activity(context.Context, model.Query) (model.Activity, error) {
	return model.Activity{}, reader.record()
}

func (reader *analyticsResponseReader) Analysis(context.Context, model.Query) (model.Analysis, error) {
	return model.Analysis{}, reader.record()
}

type analyticsResponseService struct {
	*analyticsHandlerService
	reader *analyticsResponseReader
	mu     sync.Mutex
	epoch  string
	err    error
}

func (service *analyticsResponseService) Reader() cpauk.Reader { return service.reader }

func (service *analyticsResponseService) AnalyticsResponseCacheEpoch() (string, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.epoch, service.err
}

func responseCacheHandler() (*Handler, *analyticsResponseService) {
	service := &analyticsResponseService{
		analyticsHandlerService: &analyticsHandlerService{state: model.StateReady},
		reader:                  &analyticsResponseReader{analyticsHandlerReader: &analyticsHandlerReader{}},
	}
	return &Handler{analytics: service}, service
}

func responseCacheRequest(handler *Handler, method, body, condition string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	if method == http.MethodGet {
		ctx.Request = httptest.NewRequest(method, "/v8/management/analytics/query", nil)
		ctx.Request.Header.Set(analyticsQueryHeader, base64.RawURLEncoding.EncodeToString([]byte(body)))
	} else {
		ctx.Request = httptest.NewRequest(method, "/v8/management/analytics/query", strings.NewReader(body))
	}
	ctx.Request.Header.Set("If-None-Match", condition)
	if method == http.MethodGet {
		handler.GetAnalyticsQuery(ctx)
	} else {
		handler.PostAnalyticsQuery(ctx)
	}
	ctx.Writer.WriteHeaderNow()
	return recorder
}

const analyticsNamedCacheQuery = `{"schema_version":2,"operation":"summary","range":{"preset":"last_n_hours","n":24,"time_zone":"UTC"}}`

func TestAnalyticsResponseCacheConditionalGETAndPOST(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, service := responseCacheHandler()
	first := responseCacheRequest(handler, http.MethodGet, analyticsNamedCacheQuery, "")
	if first.Code != http.StatusOK || first.Header().Get("ETag") == "" {
		t.Fatalf("first response: %d %s", first.Code, first.Body.String())
	}
	if first.Header().Get("Cache-Control") != "private, no-store" || first.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("privacy headers = %v", first.Header())
	}
	condition := `"unrelated", W/` + first.Header().Get("ETag")
	second := responseCacheRequest(handler, http.MethodGet, analyticsNamedCacheQuery, condition)
	if second.Code != http.StatusNotModified || second.Body.Len() != 0 || second.Header().Get("ETag") != first.Header().Get("ETag") {
		t.Fatalf("conditional response: %d %s %v", second.Code, second.Body.String(), second.Header())
	}
	post := responseCacheRequest(handler, http.MethodPost, analyticsNamedCacheQuery, first.Header().Get("ETag"))
	if post.Code != http.StatusOK || post.Body.String() != first.Body.String() || service.reader.calls.Load() != 1 {
		t.Fatalf("POST response: %d %s reads=%d", post.Code, post.Body.String(), service.reader.calls.Load())
	}
}

func TestAnalyticsResponseCacheQuantizesRanges(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, form := range []string{"explicit", "custom", "custom_offset", "subminute"} {
		t.Run(form, func(t *testing.T) {
			handler, service := responseCacheHandler()
			now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
			handler.analyticsResponseCache = &analyticsResponseCache{now: func() time.Time { return now }}
			start, end := "2026-08-01T00:00:01.123456789Z", "2026-08-02T00:00:01.123456789Z"
			nextStart, nextEnd := "2026-08-01T00:00:59.999999999Z", "2026-08-02T00:00:59.999999999Z"
			if form == "subminute" {
				end = "2026-08-01T00:00:02.123456789Z"
				nextStart, nextEnd = "2026-08-01T00:00:58Z", "2026-08-01T00:00:59Z"
			}
			if form == "custom_offset" {
				start, end = "2026-08-01T07:00:01.123456789+07:00", "2026-08-02T07:00:01.123456789+07:00"
			}
			body := func(start, end string) string {
				if strings.HasPrefix(form, "custom") {
					return fmt.Sprintf(`{"schema_version":2,"operation":"summary","range":{"preset":"custom","start":%q,"end":%q,"time_zone":"Asia/Bangkok"}}`, start, end)
				}
				return fmt.Sprintf(`{"schema_version":2,"operation":"summary","start":%q,"end":%q,"time_zone":"Asia/Bangkok"}`, start, end)
			}
			first := responseCacheRequest(handler, http.MethodGet, body(start, end), "")
			if first.Code != http.StatusOK {
				t.Fatalf("first request: %d %s", first.Code, first.Body.String())
			}
			var summary model.Summary
			if err := json.Unmarshal(first.Body.Bytes(), &summary); err != nil {
				t.Fatal(err)
			}
			wantStart, _ := time.Parse(time.RFC3339Nano, start)
			wantEnd, _ := time.Parse(time.RFC3339Nano, end)
			if !summary.Meta.Range.Start.Equal(wantStart) || !summary.Meta.Range.End.Equal(wantEnd) {
				t.Fatalf("computation bounds changed: %+v", summary.Meta.Range)
			}
			etag := first.Header().Get("ETag")
			now = now.Add(time.Minute - time.Nanosecond)
			hit := responseCacheRequest(handler, http.MethodGet, body(nextStart, nextEnd), etag)
			if hit.Code != http.StatusNotModified || hit.Body.Len() != 0 || service.reader.calls.Load() != 1 {
				t.Fatalf("sub-minute conditional request: %d %s reads=%d", hit.Code, hit.Body.String(), service.reader.calls.Load())
			}
			post := responseCacheRequest(handler, http.MethodPost, body(nextStart, nextEnd), etag)
			if post.Code != http.StatusOK || post.Body.String() != first.Body.String() || service.reader.calls.Load() != 1 {
				t.Fatalf("POST did not reuse snapshot: %d %s", post.Code, post.Body.String())
			}
			now = now.Add(time.Nanosecond)
			refreshed := responseCacheRequest(handler, http.MethodGet, body(nextStart, nextEnd), etag)
			if refreshed.Code != http.StatusOK || service.reader.calls.Load() != 2 {
				t.Fatalf("expired snapshot: %d reads=%d", refreshed.Code, service.reader.calls.Load())
			}
			if err := json.Unmarshal(refreshed.Body.Bytes(), &summary); err != nil {
				t.Fatal(err)
			}
			wantStart, _ = time.Parse(time.RFC3339Nano, nextStart)
			if !summary.Meta.Range.Start.Equal(wantStart) {
				t.Fatalf("refresh kept old bounds: %+v", summary.Meta.Range)
			}
			// Moving either endpoint across a UTC minute must create another entry.
			for _, next := range [][2]string{{"2026-08-01T00:01:00Z", "2026-08-02T00:00:59Z"}, {"2026-08-01T00:00:59Z", "2026-08-02T00:01:00Z"}} {
				response := responseCacheRequest(handler, http.MethodGet, body(next[0], next[1]), refreshed.Header().Get("ETag"))
				if response.Code != http.StatusOK {
					t.Fatalf("neighbor minute reused entry: %d", response.Code)
				}
			}
			if service.reader.calls.Load() != 4 {
				t.Fatalf("neighbor minute reads=%d", service.reader.calls.Load())
			}
		})
	}
}

type analyticsCacheStoreService struct {
	*analyticsHandlerService
	database *store.Store
}

func (service *analyticsCacheStoreService) Reader() cpauk.Reader { return service.database }

func TestAnalyticsResponseCacheMinuteSnapshotKeepsExactCursorBounds(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, chtest.Handle(t), store.Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(ctx) })
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for index := 1; index <= 3; index++ {
		event := model.Event{
			SchemaVersion: 1, AttemptID: fmt.Sprintf("%032x", index), ProxyRequestID: fmt.Sprintf("%032x", index),
			RequestIDQuality: model.RequestIDSynthetic, KeyID: strings.Repeat("a", 64),
			RequestedAt: start.Add(time.Duration(index) * time.Second), Provider: "test", ExecutorType: "test",
			Model: "test", EndpointClass: "responses", Succeeded: true,
			Tokens: model.TokenUsage{Input: 1, Total: 1, Schema: "normalized-v1", Quality: model.TokenQualityExact},
		}
		if err := database.WriteBatch(ctx, []model.Event{event}); err != nil {
			t.Fatal(err)
		}
	}
	handler := &Handler{analytics: &analyticsCacheStoreService{analyticsHandlerService: &analyticsHandlerService{state: model.StateReady}, database: database}}
	query := model.Query{SchemaVersion: 2, Operation: model.OperationEvents, Start: start.Add(1500 * time.Millisecond), End: start.Add(64500 * time.Millisecond), TimeZone: "Asia/Bangkok", PageSize: 1}
	request := func(q model.Query, etag string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(q)
		if err != nil {
			t.Fatal(err)
		}
		return responseCacheRequest(handler, http.MethodGet, string(body), etag)
	}
	first := request(query, "")
	if first.Code != http.StatusOK {
		t.Fatalf("first page: %d %s", first.Code, first.Body.String())
	}
	var page model.EventPage
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.TotalCount != 2 || page.Meta.NextCursor == "" || !page.Meta.Range.Start.Equal(query.Start) {
		t.Fatalf("first page did not use exact bounds: %+v", page)
	}
	shifted := query
	shifted.Start, shifted.End = start.Add(59*time.Second), start.Add(119*time.Second)
	if hit := request(shifted, first.Header().Get("ETag")); hit.Code != http.StatusNotModified {
		t.Fatalf("same-minute first page was recomputed: %d %s", hit.Code, hit.Body.String())
	}
	query.Start, query.End, query.Cursor = page.Meta.Range.Start, page.Meta.Range.End, page.Meta.NextCursor
	second := request(query, "")
	if second.Code != http.StatusOK {
		t.Fatalf("continuation: %d %s", second.Code, second.Body.String())
	}
	if err := json.Unmarshal(second.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].AttemptID != fmt.Sprintf("%032x", 2) {
		t.Fatalf("wrong continuation: %+v", page)
	}
	query.Start = query.Start.Add(time.Millisecond)
	invalid := request(query, second.Header().Get("ETag"))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("changed cursor bounds bypassed validation: %d %s", invalid.Code, invalid.Body.String())
	}
}

func TestAnalyticsResponseCacheCanonicalScopesAndAllOperations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	keyA, keyB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, operation := range []string{"summary", "timeseries", "dimensions", "events", "leaderboard", "activity", "analysis"} {
		t.Run(operation, func(t *testing.T) {
			handler, service := responseCacheHandler()
			query := map[string]any{
				"schema_version": 2, "operation": operation,
				"start": "2026-08-01T00:00:01.123Z", "end": "2026-08-02T00:00:01.123Z", "time_zone": "UTC",
				"key_ids": []string{keyA, keyB}, "filters": map[string]any{"provider": []string{"a", "b"}},
			}
			switch operation {
			case "timeseries":
				query["bucket_width"] = "1h"
			case "dimensions":
				query["dimension"] = "provider"
			case "leaderboard":
				query["sort_by"] = "tokens"
			case "activity":
				query["window"] = "day"
			}
			encoded, _ := json.Marshal(query)
			first := responseCacheRequest(handler, http.MethodGet, string(encoded), "")
			if first.Code != http.StatusOK {
				t.Fatalf("%d %s", first.Code, first.Body.String())
			}
			query["start"], query["end"] = "2026-08-01T00:00:59.999Z", "2026-08-02T00:00:59.999Z"
			query["key_ids"] = []string{keyB, keyA}
			query["filters"] = map[string]any{"provider": []string{"b", "a", "a"}}
			encoded, _ = json.Marshal(query)
			reordered := responseCacheRequest(handler, http.MethodGet, string(encoded), first.Header().Get("ETag"))
			if reordered.Code != http.StatusNotModified || service.reader.calls.Load() != 1 {
				t.Fatalf("reordered response %d reads=%d", reordered.Code, service.reader.calls.Load())
			}
			for _, field := range []string{"key_ids", "time_zone", "filters"} {
				changed := make(map[string]any, len(query))
				for name, value := range query {
					changed[name] = value
				}
				switch field {
				case "key_ids":
					changed[field] = []string{keyA}
				case "time_zone":
					changed[field] = "Asia/Bangkok"
				case "filters":
					changed[field] = map[string]any{"provider": []string{"a"}}
				}
				encoded, _ = json.Marshal(changed)
				response := responseCacheRequest(handler, http.MethodGet, string(encoded), first.Header().Get("ETag"))
				if response.Code != http.StatusOK || response.Header().Get("ETag") == first.Header().Get("ETag") {
					t.Fatalf("changed %s response: %d %s", field, response.Code, response.Body.String())
				}
			}
			if service.reader.calls.Load() != 4 {
				t.Fatalf("scoped reads=%d", service.reader.calls.Load())
			}
		})
	}
}

func TestAnalyticsResponseCacheExpiryAndErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, service := responseCacheHandler()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	handler.analyticsResponseCache = &analyticsResponseCache{now: func() time.Time { return now }}
	first := responseCacheRequest(handler, http.MethodGet, analyticsNamedCacheQuery, "")
	now = now.Add(analyticsResponseCacheTTL - time.Nanosecond)
	hit := responseCacheRequest(handler, http.MethodGet, analyticsNamedCacheQuery, "")
	if hit.Body.String() != first.Body.String() || service.reader.calls.Load() != 1 {
		t.Fatalf("snapshot changed before expiry: %s", hit.Body.String())
	}
	now = now.Add(time.Nanosecond)
	service.reader.err = cpauk.ErrMaintenance
	failed := responseCacheRequest(handler, http.MethodGet, analyticsNamedCacheQuery, first.Header().Get("ETag"))
	if failed.Code != http.StatusServiceUnavailable || failed.Header().Get("ETag") != "" {
		t.Fatalf("error response: %d %s", failed.Code, failed.Body.String())
	}
	service.reader.err = nil
	refreshed := responseCacheRequest(handler, http.MethodGet, analyticsNamedCacheQuery, first.Header().Get("ETag"))
	if refreshed.Code != http.StatusOK || service.reader.calls.Load() != 3 || refreshed.Header().Get("ETag") == first.Header().Get("ETag") {
		t.Fatalf("refresh response: %d reads=%d", refreshed.Code, service.reader.calls.Load())
	}
}

func TestAnalyticsResponseCacheCoalescesParallelNamedQueries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, service := responseCacheHandler()
	service.reader.entered, service.reader.release = make(chan struct{}, 32), make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 32)
	for range 32 {
		go func() { responses <- responseCacheRequest(handler, http.MethodGet, analyticsNamedCacheQuery, "") }()
	}
	<-service.reader.entered
	close(service.reader.release)
	var first string
	for range 32 {
		response := <-responses
		if response.Code != http.StatusOK {
			t.Fatalf("%d %s", response.Code, response.Body.String())
		}
		if first == "" {
			first = response.Body.String()
		}
		if response.Body.String() != first {
			t.Fatal("parallel requests resolved different moving range bounds")
		}
	}
	if service.reader.calls.Load() != 1 {
		t.Fatalf("parallel reads=%d", service.reader.calls.Load())
	}
}

func TestAnalyticsResponseCacheInvalidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, action := range []string{"epoch", "config", "service", "maintenance", "disable"} {
		t.Run(action, func(t *testing.T) {
			handler, service := responseCacheHandler()
			first := responseCacheRequest(handler, http.MethodGet, analyticsNamedCacheQuery, "")
			switch action {
			case "epoch":
				service.epoch = "next"
			case "config":
				handler.SetConfig(&config.Config{})
			case "service":
				handler.SetAnalyticsService(service)
			case "maintenance":
				service.err = cpauk.ErrMaintenance
			case "disable":
				service.err = cpauk.ErrDisabled
			}
			next := responseCacheRequest(handler, http.MethodGet, analyticsNamedCacheQuery, first.Header().Get("ETag"))
			want := http.StatusOK
			if action == "maintenance" {
				want = http.StatusServiceUnavailable
			} else if action == "disable" {
				want = http.StatusNotFound
			}
			if next.Code != want {
				t.Fatalf("%d %s", next.Code, next.Body.String())
			}
		})
	}
}

func TestAnalyticsResponseCacheInvalidatedDuringRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, action := range []string{"epoch", "config", "service", "maintenance"} {
		t.Run(action, func(t *testing.T) {
			handler, service := responseCacheHandler()
			service.reader.entered, service.reader.release = make(chan struct{}, 1), make(chan struct{})
			response := make(chan *httptest.ResponseRecorder, 1)
			go func() { response <- responseCacheRequest(handler, http.MethodGet, analyticsNamedCacheQuery, "") }()
			<-service.reader.entered
			switch action {
			case "epoch":
				service.mu.Lock()
				service.epoch = "next"
				service.mu.Unlock()
			case "config":
				handler.SetConfig(&config.Config{})
			case "service":
				handler.SetAnalyticsService(service)
			case "maintenance":
				service.mu.Lock()
				service.err = cpauk.ErrMaintenance
				service.mu.Unlock()
			}
			close(service.reader.release)
			result := <-response
			if result.Code != http.StatusServiceUnavailable || result.Header().Get("ETag") != "" {
				t.Fatalf("stale read served after %s: %d %s", action, result.Code, result.Body.String())
			}
		})
	}
}

func TestAnalyticsResponseCacheHTTPClients(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, service := responseCacheHandler()
	router := gin.New()
	router.GET("/v8/management/analytics/query", func(c *gin.Context) {
		if !strings.HasPrefix(c.GetHeader("Authorization"), "Bearer client-") {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		handler.GetAnalyticsQuery(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	var etag string
	for index, want := range []int{http.StatusOK, http.StatusNotModified, http.StatusUnauthorized} {
		request, err := http.NewRequest(http.MethodGet, server.URL+"/v8/management/analytics/query", nil)
		if err != nil {
			t.Fatal(err)
		}
		if index < 2 {
			request.Header.Set("Authorization", fmt.Sprintf("Bearer client-%d", index))
		}
		request.Header.Set(analyticsQueryHeader, base64.RawURLEncoding.EncodeToString([]byte(analyticsNamedCacheQuery)))
		request.Header.Set("If-None-Match", etag)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		if errClose := response.Body.Close(); errClose != nil {
			t.Fatal(errClose)
		}
		if err != nil || response.StatusCode != want {
			t.Fatalf("client=%d status=%d error=%v body=%s", index, response.StatusCode, err, body)
		}
		if index == 0 {
			etag = response.Header.Get("ETag")
		} else if len(body) != 0 {
			t.Fatalf("client=%d received unexpected body=%s", index, body)
		}
	}
	if service.reader.calls.Load() != 1 {
		t.Fatalf("HTTP clients performed %d reads", service.reader.calls.Load())
	}
}

func TestAnalyticsResponseCacheRejectsInvalidGETInputs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	valid := base64.RawURLEncoding.EncodeToString([]byte(analyticsNamedCacheQuery))
	for _, kind := range []string{"missing", "multiple", "encoding", "oversize", "mixed_url", "mixed_body", "unknown", "duplicate_json"} {
		t.Run(kind, func(t *testing.T) {
			handler, service := responseCacheHandler()
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/analytics/query", nil)
			ctx.Request.Header.Set(analyticsQueryHeader, valid)
			switch kind {
			case "missing":
				ctx.Request.Header.Del(analyticsQueryHeader)
			case "multiple":
				ctx.Request.Header.Add(analyticsQueryHeader, valid)
			case "encoding":
				ctx.Request.Header.Set(analyticsQueryHeader, "%%invalid")
			case "oversize":
				ctx.Request.Header.Set(analyticsQueryHeader, strings.Repeat("a", base64.RawURLEncoding.EncodedLen(model.MaxQueryBodyBytes)+1))
			case "mixed_url":
				ctx.Request.URL.RawQuery = "key_ids=private"
			case "mixed_body":
				ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/analytics/query", strings.NewReader("{}"))
				ctx.Request.Header.Set(analyticsQueryHeader, valid)
			case "unknown", "duplicate_json":
				field := `"unknown":1,`
				if kind == "duplicate_json" {
					field = `"operation":"summary",`
				}
				ctx.Request.Header.Set(analyticsQueryHeader, base64.RawURLEncoding.EncodeToString([]byte("{"+field+analyticsNamedCacheQuery[1:])))
			}
			handler.GetAnalyticsQuery(ctx)
			if recorder.Code != http.StatusBadRequest || service.reader.calls.Load() != 0 {
				t.Fatalf("%d %s reads=%d", recorder.Code, recorder.Body.String(), service.reader.calls.Load())
			}
		})
	}
}

func TestAnalyticsResponseCacheBoundsAndCancellation(t *testing.T) {
	cache := &analyticsResponseCache{now: time.Now}
	for index := range analyticsResponseCacheMaxEntries + 1 {
		_, err := cache.do(context.Background(), fmt.Sprint(index), func() ([]byte, error) { return []byte("{}"), nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.entries) != analyticsResponseCacheMaxEntries || cache.bytes > analyticsResponseCacheMaxBytes {
		t.Fatalf("entries=%d bytes=%d", len(cache.entries), cache.bytes)
	}
	if _, ok := cache.get("0"); ok {
		t.Fatal("least recently used entry was retained")
	}
	_, err := cache.do(context.Background(), "oversize", func() ([]byte, error) { return make([]byte, analyticsResponseCacheMaxBytes+1), nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.get("oversize"); ok {
		t.Fatal("oversize response was cached")
	}
	for _, key := range []string{"large-a", "large-b"} {
		if _, err := cache.do(context.Background(), key, func() ([]byte, error) { return make([]byte, analyticsResponseCacheMaxBytes/2+1), nil }); err != nil {
			t.Fatal(err)
		}
	}
	if cache.bytes > analyticsResponseCacheMaxBytes {
		t.Fatalf("cache byte bound exceeded: %d", cache.bytes)
	}
	if _, ok := cache.get("large-a"); ok {
		t.Fatal("least recently used large entry was retained")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	release := make(chan struct{})
	_, err = cache.do(ctx, "cancelled", func() ([]byte, error) { <-release; return nil, ctx.Err() })
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request error=%v", err)
	}
}

func TestAnalyticsResponseCacheRecoversLoadPanic(t *testing.T) {
	cache := &analyticsResponseCache{now: time.Now}
	_, err := cache.do(context.Background(), "panic", func() ([]byte, error) {
		panic("private storage details")
	})
	if !errors.Is(err, cpauk.ErrInternal) {
		t.Fatalf("panic error=%v", err)
	}
	if _, ok := cache.get("panic"); ok {
		t.Fatal("failed computation was cached")
	}
	if _, err := cache.do(context.Background(), "panic", func() ([]byte, error) { return []byte("{}"), nil }); err != nil {
		t.Fatalf("computation did not recover: %v", err)
	}
}

func TestAnalyticsLegacyQueriesRemainUncached(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, service := responseCacheHandler()
	for range 2 {
		ctx, response := v2Request(http.MethodPost, "/v0/management/analytics/query", analyticsNamedCacheQuery)
		handler.PostAnalyticsQuery(ctx)
		if response.Code != http.StatusOK || response.Header().Get("ETag") != "" {
			t.Fatalf("legacy response=%d %s", response.Code, response.Body.String())
		}
	}
	if service.reader.calls.Load() != 2 {
		t.Fatalf("legacy reads=%d", service.reader.calls.Load())
	}
}
