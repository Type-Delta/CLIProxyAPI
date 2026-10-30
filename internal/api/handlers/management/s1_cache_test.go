package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

type s1CatalogService struct {
	*analyticsResponseService
	calls atomic.Int64
}

func (s *s1CatalogService) KeyCatalog(_ context.Context, query model.Query) (backend.KeyCatalogPage, error) {
	s.calls.Add(1)
	page := backend.KeyCatalogPage{Keys: []model.KeyIdentity{{KeyID: "catalog-key"}}}
	page.Meta.NextCursor = "next-page"
	return page, nil
}

func TestS1KeyCatalogResponseCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, base := responseCacheHandler()
	s := &s1CatalogService{analyticsResponseService: base}
	h.analytics = s
	request := func(cursor, etag string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/analytics/keys?start=2026-09-01T00:00:00Z&end=2026-09-06T00:00:00Z&time_zone=UTC", nil)
		c.Request.Header.Set("X-Analytics-Cursor", cursor)
		c.Request.Header.Set("If-None-Match", etag)
		h.GetAnalyticsKeys(c)
		c.Writer.WriteHeaderNow()
		return w
	}
	first := request("", "")
	if first.Code != 200 || first.Header().Get("ETag") == "" || first.Header().Get("X-Analytics-Next-Cursor") != "next-page" {
		t.Fatalf("first=%d %s %v", first.Code, first.Body, first.Header())
	}
	second := request("", first.Header().Get("ETag"))
	if second.Code != 304 || s.calls.Load() != 1 || second.Header().Get("X-Analytics-Next-Cursor") != "next-page" {
		t.Fatalf("cached=%d reads=%d %v", second.Code, s.calls.Load(), second.Header())
	}
	next := request("cursor-page-2", "")
	if next.Code != 200 || s.calls.Load() != 2 {
		t.Fatalf("cursor=%d reads=%d", next.Code, s.calls.Load())
	}
	base.mu.Lock()
	base.epoch = "lifecycle-changed"
	base.mu.Unlock()
	refreshed := request("", first.Header().Get("ETag"))
	if refreshed.Code != 200 || s.calls.Load() != 3 {
		t.Fatalf("invalidated=%d reads=%d", refreshed.Code, s.calls.Load())
	}
	// The dimensions endpoint and catalog have different result contracts.
	responseCacheRequest(h, http.MethodPost, `{"schema_version":1,"operation":"dimensions","dimension":"key","start":"2026-09-01T00:00:00Z","end":"2026-09-06T00:00:00Z","time_zone":"UTC"}`, "")
	if base.reader.calls.Load() != 1 {
		t.Fatal("catalog and dimension caches collided")
	}
}
