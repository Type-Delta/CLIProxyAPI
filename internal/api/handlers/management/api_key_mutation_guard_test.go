package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"gopkg.in/yaml.v3"
)

func TestGetAPIKeysReturnsRawKeysIDsRevisionAndDuplicateWarning(t *testing.T) {
	h := &Handler{cfg: &config.Config{SDKConfig: config.SDKConfig{APIKeys: []config.APIKeyEntry{
		{Key: " duplicate-key-with-enough-entropy "},
		{Key: "duplicate-key-with-enough-entropy"},
	}}}}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/api-keys", nil)

	h.GetAPIKeys(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("raw response security headers = %#v", recorder.Header())
	}
	var body struct {
		APIKeys    []string                 `json:"api-keys"`
		Identities []apiKeyIdentityEntry    `json:"key-identities"`
		Warnings   []apiKeyDuplicateWarning `json:"warnings"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &body); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(body.APIKeys) != 2 || body.APIKeys[0] != " duplicate-key-with-enough-entropy " {
		t.Fatalf("raw API keys = %#v", body.APIKeys)
	}
	if len(body.Identities) != 1 || !body.Identities[0].Duplicate || body.Identities[0].Status != "configured" || len(body.Identities[0].ConfigIndexes) != 2 {
		t.Fatalf("identities = %#v", body.Identities)
	}
	if len(body.Warnings) != 1 || len(body.Warnings[0].ConfigIndexes) != 2 {
		t.Fatalf("warnings = %#v", body.Warnings)
	}
}

func TestAPIKeyIdentityCatalogMarksInjectedDigestConflict(t *testing.T) {
	entries := []config.APIKeyEntry{
		{Key: "same-key-with-enough-entropy"},
		{Key: " same-key-with-enough-entropy "},
		{Key: "different-key-with-enough-entropy"},
	}
	catalog, conflicts := buildAPIKeyIdentityCatalog(entries, func(string) string {
		return strings.Repeat("c", 64)
	})
	if len(catalog) != 1 {
		t.Fatalf("catalog = %#v, want one digest group", catalog)
	}
	identity := catalog[0]
	if identity.Status != "identity_conflict" || identity.Duplicate || len(identity.ConfigIndexes) != 3 {
		t.Fatalf("identity = %#v", identity)
	}
	if len(conflicts) != 1 || len(conflicts[0]) != 3 {
		t.Fatalf("conflicts = %#v", conflicts)
	}
}

func TestAPIKeyIdentityCatalogSkipsBlankRows(t *testing.T) {
	entries := []config.APIKeyEntry{{Key: ""}, {Key: " \t "}, {Key: "configured-key-with-enough-entropy"}}
	catalog, conflicts := buildAPIKeyIdentityCatalog(entries, config.APIKeyID)
	if len(catalog) != 1 || catalog[0].ConfigIndexes[0] != 2 {
		t.Fatalf("catalog = %#v, want only config index 2", catalog)
	}
	if len(conflicts) != 0 {
		t.Fatalf("conflicts = %#v, want none", conflicts)
	}
}

func TestPutAPIKeysStructuredContractAndRevision(t *testing.T) {
	var current config.APIKeyEntry
	if errDecode := json.Unmarshal([]byte(`{"key":"limited","limits":{"max-requests":10,"future-window":"quarter"},"label":"tenant"}`), &current); errDecode != nil {
		t.Fatalf("decode fixture: %v", errDecode)
	}
	newHandler := func() *Handler {
		return &Handler{
			cfg:            &config.Config{SDKConfig: config.SDKConfig{APIKeys: []config.APIKeyEntry{current}}},
			configFilePath: writeTestConfigFile(t),
		}
	}

	t.Run("legacy flatten rejected", func(t *testing.T) {
		h := newHandler()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/api-keys", strings.NewReader(`["limited"]`))
		h.PutAPIKeys(ctx)
		if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "structured_api_keys_required") {
			t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("contract requires revision", func(t *testing.T) {
		h := newHandler()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/api-keys", strings.NewReader(`[{"key":"rotated"}]`))
		ctx.Request.Header.Set(apiKeyContractHeader, apiKeyContractV1)
		h.PutAPIKeys(ctx)
		if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "config_revision_required") {
			t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("stale revision rejected", func(t *testing.T) {
		h := newHandler()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/api-keys", strings.NewReader(`[{"key":"rotated"}]`))
		ctx.Request.Header.Set(apiKeyContractHeader, apiKeyContractV1)
		ctx.Request.Header.Set("If-Match", `"rev-stale"`)
		h.PutAPIKeys(ctx)
		if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "config_revision_mismatch") {
			t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("current revision preserves extensions", func(t *testing.T) {
		h := newHandler()
		payload := `{"items":[{"key":"limited","limits":{"max-requests":20,"future-window":"quarter"},"label":"tenant"}],"config_revision":"` + apiKeyRevision(h.cfg.APIKeys) + `"}`
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/api-keys", strings.NewReader(payload))
		ctx.Request.Header.Set(apiKeyContractHeader, apiKeyContractV1)
		h.PutAPIKeys(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
		}
		entry := h.cfg.APIKeys[0]
		if entry.Limits == nil || entry.Limits.MaxRequests != 20 || !entry.Limits.HasExtensionFields() || len(entry.ExtensionFields) != 1 {
			t.Fatalf("updated entry = %#v", entry)
		}
	})
}

func TestAPIKeyMutationsRejectNewDuplicatesButAllowLegacyRepair(t *testing.T) {
	h := &Handler{
		cfg: &config.Config{SDKConfig: config.SDKConfig{APIKeys: []config.APIKeyEntry{
			{Key: "first"}, {Key: "second"},
		}}},
		configFilePath: writeTestConfigFile(t),
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/api-keys", strings.NewReader(`{"index":1,"new":" first "}`))
	h.PatchAPIKeys(ctx)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "duplicate_api_key") {
		t.Fatalf("new duplicate response = %d %s", recorder.Code, recorder.Body.String())
	}

	h.cfg.APIKeys = []config.APIKeyEntry{{Key: "same"}, {Key: " same "}}
	recorder = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/api-keys", strings.NewReader(`{"index":1,"new":"repaired"}`))
	h.PatchAPIKeys(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("repair response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestLegacyPatchPreservesUnknownLimitFields(t *testing.T) {
	var entry config.APIKeyEntry
	if errDecode := json.Unmarshal([]byte(`{"key":"limited","limits":{"max-requests":10,"future-window":"quarter"}}`), &entry); errDecode != nil {
		t.Fatalf("decode fixture: %v", errDecode)
	}
	h := &Handler{
		cfg:            &config.Config{SDKConfig: config.SDKConfig{APIKeys: []config.APIKeyEntry{entry}}},
		configFilePath: writeTestConfigFile(t),
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/api-keys", strings.NewReader(`{"index":0,"limits":{"max-requests":20}}`))
	h.PatchAPIKeys(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("preserving PATCH response = %d %s", recorder.Code, recorder.Body.String())
	}
	if h.cfg.APIKeys[0].Limits == nil || h.cfg.APIKeys[0].Limits.MaxRequests != 20 || !h.cfg.APIKeys[0].Limits.HasExtensionFields() {
		t.Fatalf("patched limits = %#v", h.cfg.APIKeys[0].Limits)
	}

	recorder = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/api-keys", strings.NewReader(`{"index":0,"limits":null}`))
	h.PatchAPIKeys(ctx)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "structured_api_keys_required") {
		t.Fatalf("flattening PATCH response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestGuardConfigYAMLAPIKeyMutation(t *testing.T) {
	var entry config.APIKeyEntry
	if errDecode := yaml.Unmarshal([]byte("key: secret\nlimits:\n  max-requests: 1\nfuture: keep\n"), &entry); errDecode != nil {
		t.Fatalf("decode fixture: %v", errDecode)
	}
	h := &Handler{cfg: &config.Config{SDKConfig: config.SDKConfig{APIKeys: []config.APIKeyEntry{entry}}}}

	legacyRecorder := httptest.NewRecorder()
	legacy, _ := gin.CreateTestContext(legacyRecorder)
	legacy.Request = httptest.NewRequest(http.MethodPut, "/v0/management/config.yaml", nil)
	if rejected := h.GuardConfigYAMLAPIKeyMutation(legacy, []byte("api-keys:\n  - secret\n")); !rejected {
		t.Fatal("legacy full-document flatten was accepted")
	}
	if legacyRecorder.Code != http.StatusConflict || !strings.Contains(legacyRecorder.Body.String(), "structured_api_keys_required") {
		t.Fatalf("legacy response = %d %s", legacyRecorder.Code, legacyRecorder.Body.String())
	}

	currentRecorder := httptest.NewRecorder()
	current, _ := gin.CreateTestContext(currentRecorder)
	current.Request = httptest.NewRequest(http.MethodPut, "/v0/management/config.yaml", nil)
	current.Request.Header.Set(apiKeyContractHeader, apiKeyContractV1)
	current.Request.Header.Set("If-Match", apiKeyRevision(h.cfg.APIKeys))
	candidate := []byte("api-keys:\n  - key: rotated\n")
	if rejected := h.GuardConfigYAMLAPIKeyMutation(current, candidate); rejected {
		t.Fatalf("revisioned full-document update rejected: %s", currentRecorder.Body.String())
	}
}

func TestLockedConfigYAMLGuardRejectsRevisionMadeStaleBeforeWrite(t *testing.T) {
	h := &Handler{cfg: &config.Config{SDKConfig: config.SDKConfig{APIKeys: []config.APIKeyEntry{{Key: "initial-key-with-enough-entropy"}}}}}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/config.yaml", nil)
	ctx.Request.Header.Set(apiKeyContractHeader, apiKeyContractV1)
	ctx.Request.Header.Set("If-Match", apiKeyRevision(h.cfg.APIKeys))
	candidate := []byte("api-keys:\n  - replacement-key-with-enough-entropy\n")

	// This is the check the old implementation performed before validation.
	if rejected := h.GuardConfigYAMLAPIKeyMutation(ctx, candidate); rejected {
		t.Fatalf("initial guard rejected current revision: %s", recorder.Body.String())
	}

	// A hot reload or another management request wins while validation runs.
	h.mu.Lock()
	h.cfg.APIKeys = []config.APIKeyEntry{{Key: "concurrent-key-with-enough-entropy"}}
	h.mu.Unlock()

	h.mu.Lock()
	rejected := h.GuardConfigYAMLAPIKeyMutationLocked(ctx, candidate)
	h.mu.Unlock()
	if !rejected {
		t.Fatal("lock-held guard accepted a stale revision")
	}
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "config_revision_mismatch") {
		t.Fatalf("stale response = %d %s", recorder.Code, recorder.Body.String())
	}
	if got := h.cfg.APIKeys[0].Key; got != "concurrent-key-with-enough-entropy" {
		t.Fatalf("stale candidate replaced concurrent config with %q", got)
	}
}

func TestRawConfigResponsesDisableCachingAndReferrers(t *testing.T) {
	t.Run("typed config", func(t *testing.T) {
		h := &Handler{cfg: &config.Config{SDKConfig: config.SDKConfig{APIKeys: []config.APIKeyEntry{{Key: "raw-key"}}}}}
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		h.GetConfig(ctx)
		assertRawConfigHeaders(t, recorder)
	})

	t.Run("empty typed config", func(t *testing.T) {
		h := &Handler{}
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		h.GetConfig(ctx)
		assertRawConfigHeaders(t, recorder)
	})

	t.Run("yaml config", func(t *testing.T) {
		path := writeTestConfigFile(t)
		h := &Handler{configFilePath: path}
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config.yaml", nil)
		h.GetConfigYAML(ctx)
		assertRawConfigHeaders(t, recorder)
	})
}

func assertRawConfigHeaders(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := recorder.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
	}
}
