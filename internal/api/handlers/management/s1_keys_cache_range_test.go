package management

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/chstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type s1PartialCatalogService struct {
	*analyticsResponseService
	catalog *chstore.Store
}

func (s *s1PartialCatalogService) KeyCatalog(ctx context.Context, q model.Query) (backend.KeyCatalogPage, error) {
	return s.catalog.KeyCatalog(ctx, q)
}

// Minute-grouped cache keys must not let a whole-bucket page answer a
// partial-bucket key catalog request.
func TestS1KeyCatalogCacheKeepsExactRange(t *testing.T) {
	ctx := context.Background()
	dbHandle := chtest.Handle(t)
	path := filepath.Join(t.TempDir(), "analytics.db")
	s, err := chstore.Open(ctx, dbHandle, chstore.Config{Path: path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(ctx) }()
	if err = chstore.RunLegacyImport(ctx, s, path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "cpauk", "testdata", "upstream-v1.15.0", "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Events []model.Event `json:"events"`
	}
	if err = json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 0, 0, 10, 0, time.UTC)
	e := fixture.Events[0]
	e.RequestedAt = at
	if err = s.WriteBatch(ctx, []model.Event{e}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyRetention(ctx, at.Truncate(time.Hour).Add(time.Hour), 1000); err != nil {
		t.Fatal(err)
	}
	h, base := responseCacheHandler()
	h.analytics = &s1PartialCatalogService{base, s}
	request := func(partial bool) *httptest.ResponseRecorder {
		start, end := "2026-09-01T00:00:00Z", "2026-09-01T01:00:00Z"
		if partial {
			start, end = "2026-09-01T00:00:30Z", "2026-09-01T01:00:30Z"
		}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/analytics/keys?start="+start+"&end="+end+"&time_zone=UTC", nil)
		h.GetAnalyticsKeys(c)
		c.Writer.WriteHeaderNow()
		return w
	}
	first := request(false)
	if first.Code != 200 {
		t.Fatalf("first %d %s", first.Code, first.Body)
	}
	q := model.Query{SchemaVersion: 1, Operation: model.OperationDimensions, Dimension: "key", PageSize: 50, Start: at.Truncate(time.Hour).Add(30 * time.Second), End: at.Truncate(time.Hour).Add(time.Hour + 30*time.Second), TimeZone: "UTC"}
	_, directErr := s.KeyCatalog(ctx, q)
	if !errors.Is(directErr, chstore.ErrRetainedRangePartial) {
		t.Fatalf("direct error %v", directErr)
	}
	cached := request(true)
	h.mu.Lock()
	h.analyticsResponseCache = nil
	h.mu.Unlock()
	cold := request(true)
	if cached.Code != cold.Code {
		t.Errorf("cache masks retained partial-bucket error: cached=%d cold=%d", cached.Code, cold.Code)
	}
}
