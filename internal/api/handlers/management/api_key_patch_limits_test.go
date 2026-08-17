package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagelimit"
)

func TestPatchAPIKeysLimits(t *testing.T) {
	limited := []config.APIKeyEntry{{
		Key:    "existing",
		Limits: &config.KeyLimits{MaxRequests: 10, MaxTokensM: 0.5, Resets: "daily"},
	}}
	tests := []struct {
		name       string
		initial    []config.APIKeyEntry
		body       string
		wantStatus int
		want       []config.APIKeyEntry
		wantError  string
	}{
		{
			name:       "create with null old",
			body:       `{"old":null,"new":"created"}`,
			wantStatus: http.StatusOK,
			want:       []config.APIKeyEntry{{Key: "created"}},
		},
		{
			name:       "create with limits",
			body:       `{"new":"created","limits":{"max-requests":100,"max-tokens-m":1.5,"resets":"daily"}}`,
			wantStatus: http.StatusOK,
			want: []config.APIKeyEntry{{
				Key:    "created",
				Limits: &config.KeyLimits{MaxRequests: 100, MaxTokensM: 1.5, Resets: "daily"},
			}},
		},
		{
			name:       "create with resets only discards limits",
			body:       `{"new":"created","limits":{"resets":"daily"}}`,
			wantStatus: http.StatusOK,
			want:       []config.APIKeyEntry{{Key: "created"}},
		},
		{
			name:       "update replaces limits",
			initial:    limited,
			body:       `{"old":"existing","limits":{"max-tokens-m":1.5,"resets":"weekly"}}`,
			wantStatus: http.StatusOK,
			want: []config.APIKeyEntry{{
				Key:    "existing",
				Limits: &config.KeyLimits{MaxTokensM: 1.5, Resets: "weekly"},
			}},
		},
		{
			name:       "explicit null clears limits",
			initial:    limited,
			body:       `{"index":0,"limits":null}`,
			wantStatus: http.StatusOK,
			want:       []config.APIKeyEntry{{Key: "existing"}},
		},
		{
			name:       "update with resets only discards limits",
			initial:    limited,
			body:       `{"index":0,"limits":{"resets":"daily"}}`,
			wantStatus: http.StatusOK,
			want:       []config.APIKeyEntry{{Key: "existing"}},
		},
		{
			name:       "absent limits preserves during key rotation",
			initial:    limited,
			body:       `{"old":"existing","new":"rotated"}`,
			wantStatus: http.StatusOK,
			want: []config.APIKeyEntry{{
				Key:    "rotated",
				Limits: &config.KeyLimits{MaxRequests: 10, MaxTokensM: 0.5, Resets: "daily"},
			}},
		},
		{
			name:       "invalid resets leaves config unchanged",
			initial:    limited,
			body:       `{"index":0,"limits":{"max-requests":1,"resets":"yearly"}}`,
			wantStatus: http.StatusBadRequest,
			want:       limited,
			wantError:  "invalid resets value",
		},
		{
			name:       "negative max requests leaves config unchanged",
			initial:    limited,
			body:       `{"index":0,"limits":{"max-requests":-1}}`,
			wantStatus: http.StatusBadRequest,
			want:       limited,
			wantError:  "max-requests must not be negative",
		},
		{
			name:       "out of range index",
			initial:    limited,
			body:       `{"index":1,"new":"other"}`,
			wantStatus: http.StatusBadRequest,
			want:       limited,
			wantError:  "index out of range",
		},
		{
			name:       "old matches a trimmed stored key",
			initial:    []config.APIKeyEntry{{Key: " existing "}},
			body:       `{"old":"existing","new":"updated"}`,
			wantStatus: http.StatusOK,
			want:       []config.APIKeyEntry{{Key: "updated"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			initial := cloneAPIKeyEntries(test.initial)
			want := cloneAPIKeyEntries(test.want)
			h := &Handler{
				cfg:            &config.Config{SDKConfig: config.SDKConfig{APIKeys: initial}},
				configFilePath: writeTestConfigFile(t),
			}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/api-keys", strings.NewReader(test.body))

			h.PatchAPIKeys(ctx)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if !reflect.DeepEqual(h.cfg.APIKeys, want) {
				t.Fatalf("APIKeys = %#v, want %#v", h.cfg.APIKeys, want)
			}
			if test.wantError != "" && !strings.Contains(recorder.Body.String(), test.wantError) {
				t.Fatalf("response = %s, want error containing %q", recorder.Body.String(), test.wantError)
			}
		})
	}
}

func cloneAPIKeyEntries(entries []config.APIKeyEntry) []config.APIKeyEntry {
	if entries == nil {
		return nil
	}
	clone := make([]config.APIKeyEntry, len(entries))
	for index, entry := range entries {
		clone[index] = entry
		if entry.Limits != nil {
			limits := *entry.Limits
			clone[index].Limits = &limits
		}
	}
	return clone
}

func TestPatchAPIKeysMalformedLimits(t *testing.T) {
	initial := []config.APIKeyEntry{{
		Key:    "existing",
		Limits: &config.KeyLimits{MaxRequests: 10, Resets: "daily"},
	}}
	for _, body := range []string{
		`{"index":0,"limits":[]}`,
		`{"index":0,"limits":"x"}`,
		`{"index":0,"limits":{"max-requests":"ten"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			h := &Handler{
				cfg:            &config.Config{SDKConfig: config.SDKConfig{APIKeys: cloneAPIKeyEntries(initial)}},
				configFilePath: writeTestConfigFile(t),
			}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/api-keys", strings.NewReader(body))

			h.PatchAPIKeys(ctx)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "invalid limits:") {
				t.Fatalf("response = %s, want descriptive limits error", recorder.Body.String())
			}
			if !reflect.DeepEqual(h.cfg.APIKeys, initial) {
				t.Fatalf("APIKeys = %#v, want %#v", h.cfg.APIKeys, initial)
			}
		})
	}
}

func TestPutAPIKeysNormalizesEmptyLimits(t *testing.T) {
	configFilePath := writeTestConfigFile(t)
	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: configFilePath,
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/api-keys", strings.NewReader(`[{"key":"unlimited","limits":{}}]`))

	h.PutAPIKeys(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := h.cfg.APIKeys; len(got) != 1 || got[0].Limits != nil {
		t.Fatalf("APIKeys = %#v, want one unlimited entry", got)
	}
	contents, errRead := os.ReadFile(configFilePath)
	if errRead != nil {
		t.Fatalf("read saved config: %v", errRead)
	}
	if strings.Contains(string(contents), "limits:") {
		t.Fatalf("saved config = %q, want no limits mapping", contents)
	}

	getRecorder := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(getRecorder)
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/api-keys", nil)
	h.GetAPIKeys(getCtx)
	var response struct {
		APIKeys []json.RawMessage `json:"api-keys"`
	}
	if errDecode := json.Unmarshal(getRecorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode GET response: %v", errDecode)
	}
	if len(response.APIKeys) != 1 || len(response.APIKeys[0]) == 0 || response.APIKeys[0][0] != '"' {
		t.Fatalf("GET response = %s, want bare API key string", getRecorder.Body.String())
	}
}

func TestGetAPIKeysSerializesResetsOnlyLimitAsBareKey(t *testing.T) {
	h := &Handler{cfg: &config.Config{SDKConfig: config.SDKConfig{
		APIKeys: []config.APIKeyEntry{{Key: "key", Limits: &config.KeyLimits{Resets: "daily"}}},
	}}}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/api-keys", nil)

	h.GetAPIKeys(ctx)

	if got, want := recorder.Body.String(), `{"api-keys":["key"]}`; got != want {
		t.Fatalf("GET response = %s, want %s", got, want)
	}
}

func TestResetAPIKeyLimitsWithoutTrackerRecord(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{SDKConfig: config.SDKConfig{
		APIKeys: []config.APIKeyEntry{{
			Key:    "limited",
			Limits: &config.KeyLimits{MaxRequests: 1},
		}},
	}}, nil)
	tracker := usagelimit.NewTracker()
	tracker.SetLimits(map[string]usagelimit.Limits{"limited": {MaxRequests: 1}})
	h.SetUsageLimitTracker(tracker)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/api-key-limits/reset", strings.NewReader(`{"key":"limited"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.ResetAPIKeyLimits(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
}

func TestAPIKeyLimitsSaveReloadRoundTrip(t *testing.T) {
	configFilePath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configFilePath, []byte("api-keys: []\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	want := []config.APIKeyEntry{
		{Key: "unlimited"},
		{Key: "capped", Limits: &config.KeyLimits{MaxRequests: 100, MaxTokensM: 1.5, Resets: "daily"}},
	}
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: cloneAPIKeyEntries(want)}}
	if errSave := config.SaveConfigPreserveComments(configFilePath, cfg); errSave != nil {
		t.Fatalf("save config: %v", errSave)
	}

	loaded, errLoad := config.LoadConfig(configFilePath)
	if errLoad != nil {
		t.Fatalf("load config: %v", errLoad)
	}
	if !reflect.DeepEqual(loaded.APIKeys, want) {
		t.Fatalf("loaded APIKeys = %#v, want %#v", loaded.APIKeys, want)
	}
}
