package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func TestPatchAPIKeyLabelUpdateClearAndPersistence(t *testing.T) {
	key := "key-one-with-enough-entropy"
	otherKey := "key-two-with-enough-entropy"
	limits := &config.KeyLimits{MaxRequests: 12, MaxTokensM: 0.5, Resets: "daily"}
	h := &Handler{
		cfg: &config.Config{SDKConfig: config.SDKConfig{APIKeys: []config.APIKeyEntry{
			{Key: key, Label: "old label", Limits: limits},
			{Key: otherKey},
		}}},
		configFilePath: writeTestConfigFile(t),
	}
	originalID := config.APIKeyID(key)
	originalRevision := apiKeyRevision(h.cfg.APIKeys)

	patch := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/api-keys", strings.NewReader(body))
		h.PatchAPIKeys(ctx)
		return recorder
	}

	updatedLabel := "  新しいラベル 🚀  "
	recorder := patch(`{"index":0,"label":"  新しいラベル 🚀  "}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("label update status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if got := h.cfg.APIKeys[0]; got.Key != key || got.Label != updatedLabel || !reflect.DeepEqual(got.Limits, limits) {
		t.Fatalf("updated API key = %#v, want key/label/limits preserved", got)
	}
	if config.APIKeyID(h.cfg.APIKeys[0].Key) != originalID {
		t.Fatal("label update changed API key identity")
	}
	if got := apiKeyRevision(h.cfg.APIKeys); got == originalRevision || got != recorder.Header().Get("ETag")[1:len(recorder.Header().Get("ETag"))-1] {
		t.Fatalf("label update revision = %q, original=%q, etag=%q", got, originalRevision, recorder.Header().Get("ETag"))
	}

	if recorder := patch(`{"index":0}`); recorder.Code != http.StatusOK || h.cfg.APIKeys[0].Label != updatedLabel {
		t.Fatalf("label omission changed value: status=%d label=%q", recorder.Code, h.cfg.APIKeys[0].Label)
	}
	if recorder := patch(`{"index":0,"label":""}`); recorder.Code != http.StatusOK || h.cfg.APIKeys[0].Label != "" {
		t.Fatalf("label clear failed: status=%d label=%q", recorder.Code, h.cfg.APIKeys[0].Label)
	}
	loaded, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		t.Fatalf("LoadConfig() after label clear: %v", err)
	}
	if loaded.APIKeys[0].Label != "" || loaded.APIKeys[0].Key != key || loaded.APIKeys[0].Limits == nil || loaded.APIKeys[0].Limits.MaxRequests != 12 {
		t.Fatalf("persisted API key = %#v", loaded.APIKeys[0])
	}
}

func TestAPIKeyLabelUniquenessOnManagementMutations(t *testing.T) {
	h := &Handler{
		cfg: &config.Config{SDKConfig: config.SDKConfig{APIKeys: []config.APIKeyEntry{
			{Key: "first-key-with-enough-entropy", Label: "team"},
			{Key: "second-key-with-enough-entropy"},
		}}},
		configFilePath: writeTestConfigFile(t),
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/api-keys", strings.NewReader(`{"index":1,"label":"team"}`))
	h.PatchAPIKeys(ctx)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "duplicate api key label") {
		t.Fatalf("duplicate label response = %d %s", recorder.Code, recorder.Body.String())
	}
	if h.cfg.APIKeys[1].Label != "" {
		t.Fatal("duplicate label mutation changed config")
	}

	recorder = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	body := `{"items":[{"key":"first-key-with-enough-entropy","label":"one"},{"key":"second-key-with-enough-entropy","label":"one"}],"config_revision":"` + apiKeyRevision(h.cfg.APIKeys) + `"}`
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/api-keys", strings.NewReader(body))
	ctx.Request.Header.Set(apiKeyContractHeader, apiKeyContractV1)
	h.PutAPIKeys(ctx)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "duplicate api key label") {
		t.Fatalf("duplicate PUT label response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestAPIKeyIdentityAndAnalyticsCatalogExposeConfiguredLabel(t *testing.T) {
	rawKey := "key-with-a-label-and-enough-entropy"
	label := "  Операции 🚀  "
	keyID := config.APIKeyID(rawKey)
	h := &Handler{
		cfg: &config.Config{SDKConfig: config.SDKConfig{APIKeys: []config.APIKeyEntry{{Key: rawKey, Label: label}}}},
		analytics: &analyticsHandlerService{
			reader: &analyticsHandlerReader{dimensions: model.DimensionPage{Rows: []model.DimensionRow{{Value: keyID}}}},
			state:  model.StateReady,
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/api-keys", nil)
	h.GetAPIKeys(ctx)
	var response struct {
		Identities []apiKeyIdentityEntry `json:"key-identities"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode identity response: %v", err)
	}
	if recorder.Code != http.StatusOK || len(response.Identities) != 1 || response.Identities[0].Label != label {
		t.Fatalf("identity response = %d %#v", recorder.Code, response.Identities)
	}

	recorder = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/analytics/keys?start=2026-08-01T00:00:00Z&end=2026-09-01T00:00:00Z&time_zone=UTC", nil)
	h.GetAnalyticsKeys(ctx)
	var catalog struct {
		Keys []model.KeyIdentity `json:"keys"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &catalog); err != nil {
		t.Fatalf("decode analytics catalog: %v", err)
	}
	if recorder.Code != http.StatusOK || len(catalog.Keys) != 1 || catalog.Keys[0].Label != label {
		t.Fatalf("analytics catalog = %d %#v", recorder.Code, catalog.Keys)
	}
}
