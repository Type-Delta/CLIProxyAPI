package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/ingest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/store"
)

func TestAnalyticsHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	handle := chtest.Handle(t)
	db, err := handle.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	proxyID, attemptID := strings.Repeat("b", 32), strings.Repeat("a", 32)
	r := &record.Request{RequestID: "header-test", ProxyRequestID: proxyID, StartedAt: now,
		Method: "POST", URL: "/v1/chat/completions", Transport: record.TransportHTTP,
		Headers: map[string][]string{"X-Client": {"one", "two"}}, ResponseHeaders: map[string][]string{"X-Proxy": {"reply"}},
		Attempts: []record.Attempt{
			{Index: 1, StartedAt: now, Method: "POST", URL: "https://provider.test/first", Transport: record.TransportHTTP, Headers: map[string][]string{"X-Upstream": {"first"}}, ResponseHeaders: map[string][]string{"X-Reply": {"first", "again"}}},
			{Index: 2, StartedAt: now, Method: "WEBSOCKET", URL: "wss://provider.test/second", Transport: record.TransportWebsocket, Headers: map[string][]string{"X-Websocket": {"omit"}}},
			{Index: 3, StartedAt: now, Method: "POST", URL: "https://provider.test/third", Transport: record.TransportHTTP, Headers: map[string][]string{"X-Upstream": {"third"}}, ResponseHeaders: map[string][]string{"X-Reply": {"third"}}},
		}}
	if err := ingest.New(s, t.TempDir(), 0, 30).IngestRecords(ctx, []*record.Request{r}); err != nil {
		t.Fatal(err)
	}
	reader := &analyticsHandlerReader{}
	reader.events.Events = []model.Event{{AttemptID: attemptID, ProxyRequestID: proxyID}}
	h := &Handler{analytics: &analyticsHandlerService{reader: reader, state: model.StateReady}}
	h.SetClickHouse(handle)
	router := gin.New()
	router.GET("/attempts/:attempt_id/headers", h.GetAnalyticsHeaders)
	read := func(id, extra string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/attempts/"+id+"/headers?start=2026-01-01T00:00:00Z&end=2027-01-01T00:00:00Z&time_zone=UTC&"+extra, nil))
		return w
	}
	for _, tc := range []struct {
		hop, expected string
		blocks        int
	}{
		{"client_to_cpa", `"X-Client":["one","two"]`, 1},
		{"cpa_to_client", `"X-Proxy":["reply"]`, 1},
		{"cpa_to_provider", `"X-Upstream":["third"]`, 2},
		{"provider_to_cpa", `"X-Reply":["first","again"]`, 2},
	} {
		t.Run(tc.hop, func(t *testing.T) {
			w := read(attemptID, "hop="+tc.hop)
			if w.Code != 200 || !strings.Contains(w.Body.String(), tc.expected) || strings.Contains(w.Body.String(), "X-Websocket") {
				t.Fatalf("headers: %d %s", w.Code, w.Body)
			}
			if !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
				t.Fatal("header details can be cached")
			}
			if len(ojson.GetBytes(w.Body.Bytes(), "blocks").Array()) != tc.blocks {
				t.Fatalf("wrong blocks: %s", w.Body)
			}
		})
	}
	for _, tc := range []struct {
		id, params string
		status     int
	}{
		{attemptID, "hop=invalid", 400}, {"invalid", "hop=client_to_cpa", 400},
		{attemptID, "hop=client_to_cpa&page_size=1", 400},
		{strings.Repeat("c", 32), "hop=client_to_cpa", 404},
	} {
		if w := read(tc.id, tc.params); w.Code != tc.status {
			t.Fatalf("invalid read: %d %s", w.Code, w.Body)
		}
	}
	reader.events.Events[0].ProxyRequestID = strings.Repeat("f", 32)
	if w := read(attemptID, "hop=client_to_cpa"); w.Code != http.StatusOK || w.Body.String() != `{"blocks":[]}` {
		t.Fatalf("missing telemetry: %d %s", w.Code, w.Body)
	}
	r.RequestID, r.ProxyRequestID, r.Transport = "websocket", strings.Repeat("d", 32), record.TransportWebsocket
	if err := ingest.New(s, t.TempDir(), 0, 30).IngestRecords(ctx, []*record.Request{r}); err != nil {
		t.Fatal(err)
	}
	reader.events.Events[0].ProxyRequestID = r.ProxyRequestID
	if w := read(attemptID, "hop=cpa_to_provider"); w.Code != http.StatusOK || w.Body.String() != `{"blocks":[]}` {
		t.Fatalf("websocket telemetry: %d %s", w.Code, w.Body)
	}
}
