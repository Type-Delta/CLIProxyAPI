package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagelimit"
)

func TestGetAPIKeyLimitsReturnsSortedSnapshots(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tracker := usagelimit.NewTracker()
	tracker.SetLimits(map[string]usagelimit.Limits{
		"z-key": {MaxRequests: 2},
		"a-key": {MaxTokens: 10, Resets: usagelimit.PeriodDaily},
	})
	now := time.Now()
	tracker.Allow("z-key", now)
	tracker.AddTokens("a-key", 3, now)

	handler := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	handler.SetUsageLimitTracker(tracker)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/v0/management/api-key-limits", nil)
	handler.GetAPIKeyLimits(context)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var response struct {
		APIKeyLimits []apiKeyLimitsEntry `json:"api-key-limits"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(response.APIKeyLimits) != 2 {
		t.Fatalf("entry count = %d, want 2", len(response.APIKeyLimits))
	}
	if response.APIKeyLimits[0].Key != "a-key" || response.APIKeyLimits[1].Key != "z-key" {
		t.Fatalf("keys = %#v, want sorted keys", response.APIKeyLimits)
	}
	if response.APIKeyLimits[0].KeyID != config.APIKeyID("a-key") || response.APIKeyLimits[1].KeyID != config.APIKeyID("z-key") {
		t.Fatalf("key IDs = %#v", response.APIKeyLimits)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("limit response security headers = %#v", recorder.Header())
	}
	if got := response.APIKeyLimits[0].Limits; got == nil || got.Resets != "daily" || got.MaxTokens != 10 || got.TokensUsed != 3 || got.ResetAt == nil {
		t.Fatalf("a-key limits = %#v", got)
	}
	if got := response.APIKeyLimits[1].Limits; got == nil || got.Resets != "lifetime" || got.MaxRequests != 2 || got.RequestsUsed != 1 || got.ResetAt != nil {
		t.Fatalf("z-key limits = %#v", got)
	}

	var payload struct {
		APIKeyLimits []struct {
			Key    string         `json:"key"`
			Limits map[string]any `json:"limits"`
		} `json:"api-key-limits"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode JSON shape: %v", errDecode)
	}
	if _, exists := payload.APIKeyLimits[1].Limits["reset_at"]; exists {
		t.Fatalf("lifetime reset_at = %#v, want omitted", payload.APIKeyLimits[1].Limits["reset_at"])
	}
}

func TestResetAPIKeyLimits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantReset  bool
	}{
		{name: "success", body: `{"key":"limited"}`, wantStatus: http.StatusOK, wantReset: true},
		{name: "malformed", body: `{"key":`, wantStatus: http.StatusBadRequest},
		{name: "blank", body: `{"key":"  "}`, wantStatus: http.StatusBadRequest},
		{name: "unconfigured", body: `{"key":"missing"}`, wantStatus: http.StatusNotFound},
		{name: "configured unlimited", body: `{"key":"unlimited"}`, wantStatus: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := usagelimit.NewTracker()
			tracker.SetLimits(map[string]usagelimit.Limits{
				"limited":   {MaxRequests: 2},
				"unlimited": {},
			})
			tracker.Allow("limited", time.Now())
			handler := NewHandlerWithoutConfigFilePath(&config.Config{SDKConfig: config.SDKConfig{
				APIKeys: []config.APIKeyEntry{{Key: "limited"}, {Key: "unlimited"}},
			}}, nil)
			handler.SetUsageLimitTracker(tracker)
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodPost, "/v0/management/api-key-limits/reset", strings.NewReader(tt.body))
			context.Request.Header.Set("Content-Type", "application/json")

			handler.ResetAPIKeyLimits(context)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d body=%s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if tt.wantReset {
				var response struct {
					Status string `json:"status"`
				}
				if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
					t.Fatalf("decode response: %v", errDecode)
				}
				if response.Status != "ok" {
					t.Fatalf("status response = %q, want ok", response.Status)
				}
				if got := tracker.Snapshot("limited", time.Now()).RequestsUsed; got != 0 {
					t.Fatalf("RequestsUsed after reset = %d, want 0", got)
				}
			}
		})
	}
}

func TestResetAPIKeyLimitsDoesNotResetUnconfiguredTrackerKey(t *testing.T) {
	tracker := usagelimit.NewTracker()
	tracker.SetLimits(map[string]usagelimit.Limits{"deleted": {MaxRequests: 2}})
	tracker.Allow("deleted", time.Now())

	handler := NewHandlerWithoutConfigFilePath(&config.Config{SDKConfig: config.SDKConfig{
		APIKeys: []config.APIKeyEntry{{Key: "current"}},
	}}, nil)
	handler.SetUsageLimitTracker(tracker)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/v0/management/api-key-limits/reset", strings.NewReader(`{"key":"deleted"}`))
	context.Request.Header.Set("Content-Type", "application/json")

	handler.ResetAPIKeyLimits(context)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
	if got, want := recorder.Body.String(), `{"error":"API key limit not found"}`; got != want {
		t.Fatalf("response = %s, want %s", got, want)
	}
	if snapshot := tracker.Snapshot("deleted", time.Now()); snapshot == nil || snapshot.RequestsUsed != 1 {
		t.Fatalf("deleted tracker snapshot = %#v, want one request retained", snapshot)
	}
}

func TestResetAPIKeyLimitsByKeyID(t *testing.T) {
	tracker := usagelimit.NewTracker()
	tracker.SetLimits(map[string]usagelimit.Limits{"limited": {MaxRequests: 2}})
	tracker.Allow("limited", time.Now())
	handler := NewHandlerWithoutConfigFilePath(&config.Config{SDKConfig: config.SDKConfig{
		APIKeys: []config.APIKeyEntry{{Key: " limited "}},
	}}, nil)
	handler.SetUsageLimitTracker(tracker)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	body := `{"key_id":"` + config.APIKeyID("limited") + `"}`
	context.Request = httptest.NewRequest(http.MethodPost, "/v0/management/api-key-limits/reset", strings.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")

	handler.ResetAPIKeyLimits(context)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if snapshot := tracker.Snapshot("limited", time.Now()); snapshot == nil || snapshot.RequestsUsed != 0 {
		t.Fatalf("snapshot after key ID reset = %#v", snapshot)
	}
	if !strings.Contains(recorder.Body.String(), config.APIKeyID("limited")) {
		t.Fatalf("response omitted key ID: %s", recorder.Body.String())
	}
}
