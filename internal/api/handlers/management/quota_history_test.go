package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestQuotaHistoryRejectsInvalidInputs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{}
	cases := []struct{ method, url, body string }{
		{http.MethodPost, "/quota-history", `{"provider":"all","remaining_percent":1}`},
		{http.MethodPost, "/quota-history", `{"provider":"claude"}`},
		{http.MethodPost, "/quota-history", `{"provider":"claude","remaining_percent":-1}`},
		{http.MethodPost, "/quota-history", strings.Repeat("x", 32769)},
		{http.MethodGet, "/quota-history?provider=claude&start=bad&end=bad", ""},
		{http.MethodGet, "/quota-history?provider=claude&start=2026-01-01T00:00:00Z&end=2027-01-01T00:00:00Z", ""},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.url+tc.body[:min(len(tc.body), 20)], func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(tc.method, tc.url, strings.NewReader(tc.body))
			if tc.method == http.MethodPost {
				handler.PostQuotaHistory(ctx)
			} else {
				handler.GetQuotaHistory(ctx)
			}
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestQuotaHistoryRejectsInvalidSamplesAndUsesSanitizedIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	proxy := newQuotaProxy(t)
	handler := quotaHandler(proxy)
	service := &quotaHistoryFakeService{}
	handler.SetAnalyticsService(service)
	send := func(body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/quota-history", strings.NewReader(body))
		handler.PostQuotaHistory(ctx)
		ctx.Writer.WriteHeaderNow()
		return response
	}
	for _, body := range []string{
		`{"provider":"codex","samples":[{"auth_index":"credential-a-codex","window_id":"weekly","remaining_percent":101}]}`,
		`{"provider":"codex","samples":[{"auth_index":"missing","window_id":"weekly","remaining_percent":30}]}`,
		`{"provider":"codex","samples":[{"auth_index":"credential-a-codex","window_id":"weekly","remaining_percent":30,"capacity":-1}]}`,
	} {
		if response := send(body); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid %s: status %d", body, response.Code)
		}
	}
	response := send(`{"provider":"codex","samples":[{"auth_index":"credential-a-codex","window_id":"weekly","remaining_percent":30,"plan_id":"pro"}]}`)
	if response.Code != http.StatusNoContent || len(service.samples) != 1 || service.samples[0].CredentialID != strings.Repeat("a", 64) || service.samples[0].Source != "cpamc" {
		t.Fatalf("status=%d samples=%+v", response.Code, service.samples)
	}
}

type quotaHistoryFakeService struct {
	cpauk.Service
	samples []store.QuotaHistorySample
}

func (*quotaHistoryFakeService) Capabilities() cpauk.Capabilities {
	return cpauk.Capabilities{Enabled: true, Available: true, State: model.StateReady}
}
func (*quotaHistoryFakeService) CredentialID(string, string, string) (*string, error) {
	id := strings.Repeat("a", 64)
	return &id, nil
}
func (s *quotaHistoryFakeService) RecordQuotaHistory(_ context.Context, samples []store.QuotaHistorySample) error {
	s.samples = samples
	return nil
}
func (*quotaHistoryFakeService) QuotaHistory(context.Context, string, time.Time, time.Time) ([]store.QuotaHistorySample, error) {
	return nil, nil
}

func TestQuotaHistoryProviderClassification(t *testing.T) {
	h := &Handler{}
	for _, tc := range []struct{ provider, probe, want string }{
		{"codex", "", "codex"}, {"claude", "", "claude"}, {"kimi-ai", "", "kimi"}, {"grok", "", "xai"}, {"antigravity", "", "antigravity"}, {"devin", "", "devin"}, {"meta", "", "meta"}, {"openai", "opencode-go", "opencode-go"}, {"unknown", "", ""},
	} {
		auth := &coreauth.Auth{Provider: tc.provider, Attributes: map[string]string{"usage_probe": tc.probe}}
		if got := h.quotaHistoryProvider(context.Background(), auth); got != tc.want {
			t.Errorf("%s/%s = %s, want %s", tc.provider, tc.probe, got, tc.want)
		}
	}
}

func TestQuotaHistoryReturnsCurrentSanitizedCredentialIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	proxy := newQuotaProxy(t)
	handler := quotaHandler(proxy)
	handler.SetAnalyticsService(&quotaHistoryFakeService{})
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/quota-history?provider=codex&start=2026-10-01T00:00:00Z&end=2026-10-02T00:00:00Z", nil)
	handler.GetQuotaHistory(ctx)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), strings.Repeat("a", 64)) || !strings.Contains(response.Body.String(), `"auth_index":"credential-a-codex"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
