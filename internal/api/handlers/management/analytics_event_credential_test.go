package management

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk"
	store "github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/chstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/collector"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type credentialEventService struct{ *analyticsHandlerService }

func (s *credentialEventService) CredentialID(provider, index, id string) (*string, error) {
	return model.CredentialID([]byte(strings.Repeat("k", 32)), provider, index, id)
}

func TestAnalyticsEventDetailResolvesRoutingCandidateDisplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: "credential-trace", Provider: "codex", Attributes: map[string]string{"label": "Current team label"}})
	if err != nil {
		t.Fatal(err)
	}
	service := &credentialEventService{&analyticsHandlerService{reader: &analyticsHandlerReader{}, state: model.StateReady}}
	id, err := service.CredentialID("codex", auth.Index, auth.ID)
	if err != nil {
		t.Fatal(err)
	}
	trace, err := json.Marshal(routing.Trace{Version: routing.TraceVersion, Mode: routing.ModeLocal, DecidedBy: routing.DecidedByStrategy,
		Candidates: []routing.Candidate{{ID: *id}}, Pick: 0})
	if err != nil {
		t.Fatal(err)
	}
	event := model.Event{AttemptID: strings.Repeat("a", 32), Provider: "codex", RoutingTrace: trace}
	service.reader.events.Events = []model.Event{event}
	handler := &Handler{analytics: service, authManager: manager}
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Params = gin.Params{{Key: "attempt_id", Value: event.AttemptID}}
	ctx.Request = httptest.NewRequest(http.MethodGet, "/?start=2026-08-01T00:00:00Z&end=2026-09-01T00:00:00Z&time_zone=UTC", nil)
	handler.GetAnalyticsEvent(ctx)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	var got struct {
		RoutingTrace routing.Trace `json:"routing_trace"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.RoutingTrace.Candidates) != 1 || got.RoutingTrace.Candidates[0].Label != "Current team label" || got.RoutingTrace.Candidates[0].Provider != "codex" {
		t.Fatalf("resolved routing trace = %+v", got.RoutingTrace)
	}
}

func TestAnalyticsEventDetailNormalizesNullRoutingArrays(t *testing.T) {
	event := model.Event{RoutingTrace: json.RawMessage(`{"version":1,"mode":"local","retry_round":0,"selection":0,"decided_by":"strategy","candidates":null,"rules":{"matched":null,"applied":-1,"pool":null},"tier":{"priority":0,"pool":null},"pick":-1}`)}
	resolveAnalyticsRoutingTrace(&Handler{}, nil, &event)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(event.RoutingTrace, &got); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"candidates", "rules", "tier"} {
		if string(got[field]) == "null" {
			t.Errorf("routing_trace.%s is null: %s", field, event.RoutingTrace)
		}
	}
	var candidates []json.RawMessage
	if err := json.Unmarshal(got["candidates"], &candidates); err != nil || candidates == nil {
		t.Errorf("routing_trace.candidates=%s err=%v, want empty array", got["candidates"], err)
	}
	var rules struct {
		Matched json.RawMessage `json:"matched"`
		Pool    json.RawMessage `json:"pool"`
	}
	if err := json.Unmarshal(got["rules"], &rules); err != nil || string(rules.Matched) != "[]" || string(rules.Pool) == "null" {
		t.Errorf("routing_trace.rules arrays are not normalized: matched=%s pool=%s err=%v", rules.Matched, rules.Pool, err)
	}
	var tier struct {
		Pool []int `json:"pool"`
	}
	if err := json.Unmarshal(got["tier"], &tier); err != nil || tier.Pool == nil {
		t.Errorf("routing_trace.tier.pool=%s err=%v, want empty array", got["tier"], err)
	}
}

type storedRoutingTraceService struct {
	*analyticsHandlerService
	reader   *store.Store
	identity [32]byte
}

func (s *storedRoutingTraceService) Reader() cpauk.Reader { return s.reader }
func (s *storedRoutingTraceService) EventByAttemptID(ctx context.Context, id string, query model.Query) (model.Event, bool, error) {
	return s.reader.EventByAttemptID(ctx, id, query)
}
func (s *storedRoutingTraceService) CredentialID(provider, index, id string) (*string, error) {
	return model.CredentialID(s.identity[:], provider, index, id)
}

func TestRoutingTraceUsageToCollectorStoreDetailAndExports(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	identity := [32]byte{1, 2, 3}
	database, err := store.Open(ctx, chtest.Handle(t), store.Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(ctx) }()
	collectorService, err := collector.New(database, collector.Options{Capacity: 16, BatchSize: 1, FlushInterval: time.Hour, FailureThreshold: 3})
	if err != nil {
		t.Fatal(err)
	}
	collectorService.Start()
	attemptID := strings.Repeat("c", 32)
	sanitizer := collector.NewSanitizer(collector.SanitizerOptions{IdentityKey: identity, StoreCredential: true, NewID: func() (string, error) { return attemptID, nil }})
	adapter := collector.NewAdapter(collectorService, sanitizer)
	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(ctx, &coreauth.Auth{ID: "trace-auth-id", Provider: "codex", Attributes: map[string]string{"label": "Live credential label"}})
	if err != nil {
		t.Fatal(err)
	}
	trace := &routing.Trace{Version: routing.TraceVersion, Mode: routing.ModeLocal, DecidedBy: routing.DecidedByStrategy,
		Candidates: []routing.Candidate{{ID: auth.ID, AuthIndex: auth.Index, Provider: auth.Provider}}, Pick: 0}
	requestedAt := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	adapter.HandleUsage(ctx, coreusage.Record{ProxyRequestID: strings.Repeat("d", 32), Provider: "codex", ExecutorType: "codex", Model: "model", APIKey: "native-test-key",
		AuthID: auth.ID, AuthIndex: auth.Index, AuthType: "oauth", RequestedAt: requestedAt, RoutingTrace: trace, Detail: coreusage.Detail{TotalTokens: 1}})
	if err := collectorService.Close(ctx); err != nil {
		t.Fatal(err)
	}
	service := &storedRoutingTraceService{analyticsHandlerService: &analyticsHandlerService{reader: &analyticsHandlerReader{}, state: model.StateReady}, reader: database, identity: identity}
	handler := &Handler{analytics: service, authManager: manager}
	response := httptest.NewRecorder()
	requestCtx, _ := gin.CreateTestContext(response)
	requestCtx.Params = gin.Params{{Key: "attempt_id", Value: attemptID}}
	requestCtx.Request = httptest.NewRequest(http.MethodGet, "/?start=2026-09-02T00:00:00Z&end=2026-09-03T00:00:00Z&time_zone=UTC", nil)
	handler.GetAnalyticsEvent(requestCtx)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	var got struct {
		RoutingTrace routing.Trace `json:"routing_trace"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.RoutingTrace.Candidates) != 1 || got.RoutingTrace.Candidates[0].Label != "Live credential label" || got.RoutingTrace.Candidates[0].Provider != "codex" || got.RoutingTrace.Candidates[0].ID != *mustCredentialID(t, identity, auth) {
		t.Fatalf("end-to-end routing trace = %+v", got.RoutingTrace)
	}

	listCtx, listRecorder := v2Request(http.MethodGet, "/v8/management/analytics/events?start=2026-09-02T00:00:00Z&end=2026-09-03T00:00:00Z&time_zone=UTC", "")
	handler.GetAnalyticsEvents(listCtx)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status %d: %s", listRecorder.Code, listRecorder.Body.String())
	}
	var listed model.EventPage
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &listed); err != nil || len(listed.Events) != 1 || len(listed.Events[0].RoutingTrace) != 0 {
		t.Fatalf("event list returned routing trace: events=%+v err=%v", listed.Events, err)
	}

	queryJSON := `{"schema_version":2,"operation":"events","start":"2026-09-02T00:00:00Z","end":"2026-09-03T00:00:00Z","time_zone":"UTC"}`
	for _, format := range []string{"json", "csv"} {
		exportCtx, exportRecorder := v2Request(http.MethodPost, "/v8/management/analytics/exports", `{"query":`+queryJSON+`,"format":"`+format+`"}`)
		handler.CreateAnalyticsExport(exportCtx)
		if exportRecorder.Code != http.StatusOK {
			t.Fatalf("%s export status %d: %s", format, exportRecorder.Code, exportRecorder.Body.String())
		}
		if format == "json" {
			var exported []map[string]json.RawMessage
			if err := json.Unmarshal(exportRecorder.Body.Bytes(), &exported); err != nil || len(exported) != 1 || len(exported[0]["routing_trace"]) == 0 || string(exported[0]["routing_trace"]) == "null" {
				t.Fatalf("JSON export omitted routing trace: %s err=%v", exportRecorder.Body.String(), err)
			}
			var persisted routing.Trace
			if err := json.Unmarshal(exported[0]["routing_trace"], &persisted); err != nil || len(persisted.Candidates) != 1 || persisted.Candidates[0].Label != "" {
				t.Fatalf("export trace is not the persisted form: %+v err=%v", persisted, err)
			}
		} else {
			rows, err := csv.NewReader(strings.NewReader(exportRecorder.Body.String())).ReadAll()
			if err != nil || len(rows) != 2 {
				t.Fatalf("CSV export rows=%v err=%v", rows, err)
			}
			index := -1
			for i, field := range rows[0] {
				if field == "routing_trace" {
					index = i
				}
			}
			if index < 0 || rows[1][index] == "" {
				t.Fatalf("CSV export omitted routing trace: %v", rows)
			}
		}
	}
}

func mustCredentialID(t *testing.T, identity [32]byte, auth *coreauth.Auth) *string {
	t.Helper()
	id, err := model.CredentialID(identity[:], auth.Provider, auth.Index, auth.ID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAnalyticsEventDetailResolvesCredentialFilename(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name, filename, provider string
		want                     *string
	}{
		{"filename", "/private/auths/codex-test.json", "codex", new("codex-test.json")},
		{"windows filename", `C:\private\auths\codex-test.json`, "codex", new("codex-test.json")},
		{"no backing file", "", "codex", nil},
		{"different provider", "other.json", "claude", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: "credential-test", Provider: test.provider, FileName: test.filename})
			if err != nil {
				t.Fatal(err)
			}
			service := &credentialEventService{&analyticsHandlerService{reader: &analyticsHandlerReader{}, state: model.StateReady}}
			id, err := service.CredentialID("codex", auth.Index, auth.ID)
			if err != nil {
				t.Fatal(err)
			}
			event := model.Event{AttemptID: strings.Repeat("a", 32), Provider: "codex", CredentialID: id}
			service.reader.events.Events = []model.Event{event}
			handler := &Handler{analytics: service, authManager: manager}
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Params = gin.Params{{Key: "attempt_id", Value: event.AttemptID}}
			ctx.Request = httptest.NewRequest(http.MethodGet, "/?start=2026-08-01T00:00:00Z&end=2026-09-01T00:00:00Z&time_zone=UTC", nil)
			handler.GetAnalyticsEvent(ctx)
			if response.Code != http.StatusOK {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			var got struct {
				Filename *string `json:"credential_filename"`
				ID       *string `json:"credential_id"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if (got.Filename == nil) != (test.want == nil) || (got.Filename != nil && *got.Filename != *test.want) {
				t.Fatalf("filename = %v, want %v", got.Filename, test.want)
			}
			if got.ID == nil || *got.ID != *id {
				t.Fatal("hashed identity changed")
			}
		})
	}
}

func TestAnalyticsDimensionsResolveCredentialDisplayNames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "credential-dimension", Provider: "codex", FileName: "/private/auths/codex-test.json",
		Attributes: map[string]string{"label": "Team Codex"},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &credentialEventService{&analyticsHandlerService{reader: &analyticsHandlerReader{}, state: model.StateReady}}
	id, err := service.CredentialID("codex", auth.Index, auth.ID)
	if err != nil {
		t.Fatal(err)
	}
	service.reader.dimensions = model.DimensionPage{Rows: []model.DimensionRow{{Value: *id}}}
	for _, test := range []struct {
		name    string
		manager *coreauth.Manager
		want    bool
	}{
		{name: "admin", manager: manager, want: true},
		{name: "unloaded manager", manager: nil, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			service.reader.dimensions = model.DimensionPage{Rows: []model.DimensionRow{{Value: *id}}}
			handler := &Handler{analytics: service, authManager: test.manager}
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/?start=2026-08-01T00:00:00Z&end=2026-09-01T00:00:00Z&time_zone=UTC&dimension=credential", nil)
			handler.GetAnalyticsDimensions(ctx)
			if response.Code != http.StatusOK {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			var got struct {
				Rows []struct {
					Filename *string `json:"credential_filename"`
					Label    *string `json:"credential_label"`
				} `json:"rows"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Rows) != 1 {
				t.Fatalf("rows = %+v", got.Rows)
			}
			if !test.want {
				if got.Rows[0].Filename != nil || got.Rows[0].Label != nil {
					t.Fatalf("unnamed auth manager leaked display fields: %+v", got.Rows[0])
				}
				return
			}
			if got.Rows[0].Filename == nil || *got.Rows[0].Filename != "codex-test.json" || got.Rows[0].Label == nil || *got.Rows[0].Label != "Team Codex" {
				t.Fatalf("display fields = %+v", got.Rows[0])
			}
		})
	}
}
