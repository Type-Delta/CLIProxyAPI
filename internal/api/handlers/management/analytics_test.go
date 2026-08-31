package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type analyticsHandlerReader struct {
	query       model.Query
	summary     model.Summary
	timeseries  model.Timeseries
	dimensions  model.DimensionPage
	events      model.EventPage
	leaderboard model.LeaderboardPage
	err         error
}

func (r *analyticsHandlerReader) Summary(_ context.Context, query model.Query) (model.Summary, error) {
	r.query = query
	return r.summary, r.err
}

func (r *analyticsHandlerReader) Timeseries(_ context.Context, query model.Query) (model.Timeseries, error) {
	r.query = query
	return r.timeseries, r.err
}

func (r *analyticsHandlerReader) Dimensions(_ context.Context, query model.Query) (model.DimensionPage, error) {
	r.query = query
	return r.dimensions, r.err
}

func (r *analyticsHandlerReader) Events(_ context.Context, query model.Query) (model.EventPage, error) {
	r.query = query
	return r.events, r.err
}

func (r *analyticsHandlerReader) Leaderboard(_ context.Context, query model.Query) (model.LeaderboardPage, error) {
	r.query = query
	return r.leaderboard, r.err
}

type analyticsHandlerMaintenance struct{}

func (analyticsHandlerMaintenance) Start(context.Context, cpauk.MaintenanceRequest) (model.JobStatus, error) {
	return model.JobStatus{}, cpauk.ErrUnavailable
}
func (analyticsHandlerMaintenance) Status(context.Context, string) (model.JobStatus, error) {
	return model.JobStatus{}, cpauk.ErrUnavailable
}
func (analyticsHandlerMaintenance) Cancel(context.Context, string) error { return cpauk.ErrUnavailable }

type analyticsHandlerService struct {
	reader *analyticsHandlerReader
	state  model.AnalyticsState
}

type analyticsPricingTestService struct {
	*analyticsHandlerService
	book aggregate.PriceBook
}

func (s *analyticsPricingTestService) PriceBook(context.Context) (aggregate.PriceBook, error) {
	return s.book, nil
}

func (s *analyticsPricingTestService) UpdatePriceBook(_ context.Context, book aggregate.PriceBook) (aggregate.PriceBook, error) {
	s.book = book
	return book, nil
}

func (s *analyticsHandlerService) Observer() coreusage.Plugin { return nil }
func (s *analyticsHandlerService) Reader() cpauk.Reader       { return s.reader }
func (s *analyticsHandlerService) Maintenance() cpauk.Maintenance {
	return analyticsHandlerMaintenance{}
}
func (s *analyticsHandlerService) Capabilities() cpauk.Capabilities {
	return model.Capabilities{Supported: true, Enabled: s.state != model.StateDisabled, Available: s.state == model.StateReady, State: s.state}
}
func (s *analyticsHandlerService) Health() cpauk.Health { return model.Health{State: s.state} }
func (s *analyticsHandlerService) Reconfigure(cpauk.Config) cpauk.ReconfigureResult {
	return cpauk.ReconfigureResult{}
}
func (s *analyticsHandlerService) Retry(context.Context) error { return nil }
func (s *analyticsHandlerService) Close(context.Context) error { return nil }
func (s *analyticsHandlerService) KeyCatalog(_ context.Context, query model.Query) (store.KeyCatalogPage, error) {
	s.reader.query = query
	page := store.KeyCatalogPage{Meta: s.reader.dimensions.Meta}
	for _, row := range s.reader.dimensions.Rows {
		page.Keys = append(page.Keys, model.KeyIdentity{
			KeyID: row.Value, TotalTokens: row.Tokens.Total, KnownCost: row.KnownCost, UnpricedTokens: row.UnpricedTokens,
		})
	}
	return page, s.reader.err
}

func (s *analyticsHandlerService) EventByAttemptID(_ context.Context, attemptID string, query model.Query) (model.Event, bool, error) {
	s.reader.query = query
	if s.reader.err != nil {
		return model.Event{}, false, s.reader.err
	}
	for _, event := range s.reader.events.Events {
		if event.AttemptID == attemptID {
			return event, true, nil
		}
	}
	return model.Event{}, false, nil
}

func TestPostAnalyticsQueryPassesBodyKeyScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reader := &analyticsHandlerReader{}
	handler := &Handler{analytics: &analyticsHandlerService{reader: reader, state: model.StateReady}}
	keyA := strings.Repeat("a", 64)
	keyB := strings.Repeat("b", 64)
	body := `{"schema_version":1,"operation":"summary","start":"2026-08-01T00:00:00Z","end":"2026-09-01T00:00:00Z","time_zone":"UTC","key_ids":["` + keyA + `","` + keyB + `"]}`
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/analytics/query", strings.NewReader(body))
	handler.PostAnalyticsQuery(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(reader.query.KeyIDs) != 2 || reader.query.KeyIDs[0] != keyA || reader.query.KeyIDs[1] != keyB {
		t.Fatalf("key scope = %#v", reader.query.KeyIDs)
	}
}

func TestAnalyticsEventDetailRejectsPaginationParameters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{analytics: &analyticsHandlerService{reader: &analyticsHandlerReader{}, state: model.StateReady}}
	for _, parameter := range []string{"cursor=garbage", "page_size=100"} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Params = gin.Params{{Key: "attempt_id", Value: strings.Repeat("a", 32)}}
		ctx.Request = httptest.NewRequest(http.MethodGet,
			"/?start=2026-08-01T00:00:00Z&end=2026-09-01T00:00:00Z&time_zone=UTC&"+parameter, nil)
		handler.GetAnalyticsEvent(ctx)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), string(model.ErrorAnalyticsInvalidQuery)) {
			t.Fatalf("%s status=%d body=%s", parameter, recorder.Code, recorder.Body.String())
		}
	}
}

func TestAnalyticsStateErrorsUseFrozenEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name string
		err  error
		want int
		code model.ErrorCode
	}{
		{name: "disabled", err: cpauk.ErrDisabled, want: http.StatusNotFound, code: model.ErrorAnalyticsDisabled},
		{name: "unavailable", err: cpauk.ErrUnavailable, want: http.StatusServiceUnavailable, code: model.ErrorAnalyticsUnavailable},
		{name: "maintenance", err: cpauk.ErrMaintenance, want: http.StatusServiceUnavailable, code: model.ErrorAnalyticsMaintenance},
		{name: "internal", err: errors.New("secret database path /tmp/private"), want: http.StatusInternalServerError, code: model.ErrorAnalyticsInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &analyticsHandlerReader{err: test.err}
			handler := &Handler{analytics: &analyticsHandlerService{reader: reader, state: model.StateReady}}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			requestID := strings.Repeat("f", 32)
			request := httptest.NewRequest(http.MethodGet, "/?start=2026-08-01T00:00:00Z&end=2026-09-01T00:00:00Z&time_zone=UTC", nil)
			ctx.Request = request.WithContext(coreusage.WithProxyRequestID(request.Context(), requestID))
			handler.GetAnalyticsSummary(ctx)
			if recorder.Code != test.want || !strings.Contains(recorder.Body.String(), string(test.code)) {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "/tmp/private") {
				t.Fatal("internal error text reached response")
			}
			if !strings.Contains(recorder.Body.String(), requestID) {
				t.Fatalf("proxy request ID missing from error: %s", recorder.Body.String())
			}
		})
	}
}

func TestAnalyticsKeyCatalogContainsHashesOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rawKey := "raw-admin-secret"
	keyID := config.APIKeyID(rawKey)
	reader := &analyticsHandlerReader{dimensions: model.DimensionPage{Rows: []model.DimensionRow{{Value: keyID, Tokens: model.TokenUsage{Total: 42}}}}}
	handler := &Handler{
		cfg:       &config.Config{SDKConfig: config.SDKConfig{APIKeys: []config.APIKeyEntry{{Key: rawKey}}}},
		analytics: &analyticsHandlerService{reader: reader, state: model.StateReady},
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/?start=2026-08-01T00:00:00Z&end=2026-09-01T00:00:00Z&time_zone=UTC", nil)
	handler.GetAnalyticsKeys(ctx)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), keyID) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), rawKey) {
		t.Fatal("raw key reached analytics catalog")
	}
}

func TestAnalyticsCSVFormulaEscaping(t *testing.T) {
	reader := &analyticsHandlerReader{events: model.EventPage{Events: []model.Event{{
		AttemptID: strings.Repeat("a", 32), ProxyRequestID: strings.Repeat("b", 32), KeyID: strings.Repeat("c", 64),
		RequestedAt: time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), Provider: "=2+2", Model: "+cmd",
		EndpointClass: "@SUM(A1)", Tokens: model.TokenUsage{Total: 10},
	}}}}
	query := model.Query{
		SchemaVersion: 1, Operation: model.OperationEvents, Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		End: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), TimeZone: "UTC", PageSize: 100,
	}
	var output bytes.Buffer
	if _, err := writeAnalyticsEventsCSV(context.Background(), &output, reader, query, 100); err != nil {
		t.Fatal(err)
	}
	for _, dangerous := range []string{",=2+2,", ",+cmd,", ",@SUM(A1),"} {
		if strings.Contains(output.String(), dangerous) {
			t.Fatalf("formula marker was not escaped: %q", output.String())
		}
	}
}

func TestAnalyticsPricingUsesExactFrozenRuleShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &analyticsPricingTestService{analyticsHandlerService: &analyticsHandlerService{reader: &analyticsHandlerReader{}, state: model.StateReady}}
	handler := &Handler{analytics: service}
	body := `{"currency_unit":"nano_usd","rounding":"half_away_from_zero_once_per_event","rules":[{"rule_id":"price-1","match":{"model":"model-a"},"input_per_million_usd":"10.035","output_per_million_usd":"15.0525","cache_read_multiplier":"0","cache_creation_multiplier":"0","source":"admin"}]}`
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/analytics/pricing", strings.NewReader(body))
	handler.PutAnalyticsPricing(ctx)
	if recorder.Code != http.StatusOK || len(service.book.Rules) != 1 || service.book.Rules[0].InputPerMillion == nil || service.book.Rules[0].InputPerMillion.String() != "10.035" {
		t.Fatalf("status=%d body=%s book=%#v", recorder.Code, recorder.Body.String(), service.book)
	}
	if !strings.Contains(recorder.Body.String(), `"input_per_million_usd":"10.035"`) || strings.Contains(recorder.Body.String(), "provider") {
		t.Fatalf("pricing response does not match fixture shape: %s", recorder.Body.String())
	}
}

func TestAnalyticsViewerStorePersistsHashesAndRevokesSessions(t *testing.T) {
	path := t.TempDir() + "/viewers.json"
	store, err := NewAnalyticsViewerStore(path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(ViewerCreateRequest{
		KeyID: strings.Repeat("d", 64), AllowedViews: []string{"summary", "events"},
		ExpiresAt: time.Now().UTC().Add(time.Hour), Label: "team",
	})
	if err != nil {
		t.Fatal(err)
	}
	token, scope, err := store.Exchange(created.Credential)
	if err != nil || scope.KeyID != strings.Repeat("d", 64) {
		t.Fatalf("exchange scope = %#v, err = %v", scope, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(created.Credential)) || bytes.Contains(data, []byte(token)) {
		t.Fatal("viewer store persisted a raw credential or session")
	}
	restarted, err := NewAnalyticsViewerStore(path)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := restarted.List()
	if err != nil || len(listed) != 1 || listed[0].ID != created.ID || listed[0].KeyID != scope.KeyID {
		t.Fatalf("durable viewer list=%#v err=%v", listed, err)
	}
	listedJSON, err := json.Marshal(listed)
	if err != nil || bytes.Contains(listedJSON, []byte(created.Credential)) || bytes.Contains(listedJSON, []byte("credential_hash")) {
		t.Fatalf("viewer metadata exposed a credential: %s err=%v", listedJSON, err)
	}
	if restartedScope, errAuth := restarted.Authenticate(token, "summary"); errAuth != nil || restartedScope.KeyID != scope.KeyID {
		t.Fatalf("durable session scope = %#v, err = %v", restartedScope, errAuth)
	}
	if runtime.GOOS != "windows" {
		info, errInfo := os.Stat(path)
		if errInfo != nil {
			t.Fatal(errInfo)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("viewer store mode = %v", info.Mode().Perm())
		}
	}
	if _, err = store.Authenticate(token, "timeseries"); !errors.Is(err, ErrViewerViewForbidden) {
		t.Fatalf("cross-view auth err = %v", err)
	}
	if err = store.InvalidateSessions(); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Authenticate(token, "summary"); !errors.Is(err, ErrViewerSessionInvalid) {
		t.Fatalf("invalidated session err = %v", err)
	}
	newToken, _, err := store.Exchange(created.Credential)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Revoke(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Authenticate(newToken, "summary"); !errors.Is(err, ErrViewerSessionInvalid) {
		t.Fatalf("revoked session err = %v", err)
	}
	reloaded, err := NewAnalyticsViewerStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = reloaded.Exchange(created.Credential); !errors.Is(err, ErrViewerCredentialInvalid) {
		t.Fatalf("revoked credential survived reload: %v", err)
	}
}

func TestCreateAnalyticsViewerMapsPersistenceFailureToInternalError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	directory := t.TempDir()
	store, err := NewAnalyticsViewerStore("")
	if err != nil {
		t.Fatal(err)
	}
	blockedParent := filepath.Join(directory, "not-a-directory")
	if err = os.WriteFile(blockedParent, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(blockedParent, "viewers.json")
	handler := &Handler{analytics: &analyticsHandlerService{reader: &analyticsHandlerReader{}, state: model.StateReady}, analyticsViewers: store}
	body := fmt.Sprintf(`{"key_id":%q,"allowed_views":["summary"],"expires_at":%q}`,
		strings.Repeat("e", 64), time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/analytics/viewers", strings.NewReader(body))
	handler.CreateAnalyticsViewer(ctx)
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), string(model.ErrorAnalyticsInternal)) {
		t.Fatalf("persistence failure status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestViewerCreateResponseRoundTripDoesNotExposeKeyIDInSession(t *testing.T) {
	store, err := NewAnalyticsViewerStore("")
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(ViewerCreateRequest{
		KeyID: strings.Repeat("e", 64), AllowedViews: []string{"summary"}, ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(created)
	if err != nil || !bytes.Contains(encoded, []byte(`"credential"`)) {
		t.Fatalf("create response = %s, err = %v", encoded, err)
	}
	_, scope, err := store.Exchange(created.Credential)
	if err != nil || scope.KeyID == "" {
		t.Fatalf("scope = %#v, err = %v", scope, err)
	}
}

func TestViewerStoreRollsBackRevocationWhenPersistenceFails(t *testing.T) {
	directory := t.TempDir()
	store, err := NewAnalyticsViewerStore(filepath.Join(directory, "viewers.json"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(ViewerCreateRequest{
		KeyID: strings.Repeat("e", 64), AllowedViews: []string{"summary"}, ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.Exchange(created.Credential)
	if err != nil {
		t.Fatal(err)
	}
	blockedParent := filepath.Join(directory, "not-a-directory")
	if err = os.WriteFile(blockedParent, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(blockedParent, "viewers.json")
	if err = store.Revoke(created.ID); err == nil {
		t.Fatal("revoke unexpectedly persisted")
	}
	if _, err = store.Authenticate(token, "summary"); err != nil {
		t.Fatalf("failed revoke changed live state: %v", err)
	}
	if err = store.InvalidateSessions(); err == nil {
		t.Fatal("session invalidation unexpectedly persisted")
	}
	if _, err = store.Authenticate(token, "summary"); err != nil {
		t.Fatalf("failed session invalidation changed live state: %v", err)
	}
}

func TestViewerStoreRejectsOversizedDocumentBeforeReadingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "viewers.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(viewerStoreMaxBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = NewAnalyticsViewerStore(path); err == nil || !strings.Contains(err.Error(), "exceeds its bounds") {
		t.Fatalf("oversized viewer store error=%v", err)
	}
}
