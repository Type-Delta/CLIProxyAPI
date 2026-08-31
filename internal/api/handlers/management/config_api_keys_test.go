package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestGetAPIKeysKeepsUnlimitedWireFormat(t *testing.T) {
	h := &Handler{cfg: &config.Config{SDKConfig: config.SDKConfig{
		APIKeys: []config.APIKeyEntry{{Key: "first"}, {Key: "second"}},
	}}}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/api-keys", nil)

	h.GetAPIKeys(ctx)

	var response struct {
		APIKeys        []string              `json:"api-keys"`
		Identities     []apiKeyIdentityEntry `json:"key-identities"`
		ConfigRevision string                `json:"config_revision"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(response.APIKeys) != 2 || response.APIKeys[0] != "first" || response.APIKeys[1] != "second" {
		t.Fatalf("API keys = %#v", response.APIKeys)
	}
	if len(response.Identities) != 2 || response.Identities[0].KeyID != config.APIKeyID("first") {
		t.Fatalf("key identities = %#v", response.Identities)
	}
	if response.ConfigRevision == "" || recorder.Header().Get("ETag") == "" {
		t.Fatal("GET omitted configuration revision")
	}
}

func TestAPIKeyMutationsPreserveLimitsForUnchangedPlainKey(t *testing.T) {
	h := &Handler{
		cfg: &config.Config{SDKConfig: config.SDKConfig{
			APIKeys: []config.APIKeyEntry{{Key: "limited", Limits: &config.KeyLimits{MaxRequests: 10}}},
		}},
		configFilePath: writeTestConfigFile(t),
	}

	putRecorder := httptest.NewRecorder()
	putCtx, _ := gin.CreateTestContext(putRecorder)
	putCtx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/api-keys", strings.NewReader(`["limited","new"]`))
	h.PutAPIKeys(putCtx)
	if putRecorder.Code != http.StatusConflict {
		t.Fatalf("legacy PUT status = %d, want %d; body=%s", putRecorder.Code, http.StatusConflict, putRecorder.Body.String())
	}

	putRecorder = httptest.NewRecorder()
	putCtx, _ = gin.CreateTestContext(putRecorder)
	putCtx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/api-keys", strings.NewReader(`[{"key":"limited","limits":{"max-requests":10}},"new"]`))
	putCtx.Request.Header.Set(apiKeyContractHeader, apiKeyContractV1)
	putCtx.Request.Header.Set("If-Match", apiKeyRevision(h.cfg.APIKeys))
	h.PutAPIKeys(putCtx)
	if putRecorder.Code != http.StatusOK {
		t.Fatalf("structured PUT status = %d, want %d; body=%s", putRecorder.Code, http.StatusOK, putRecorder.Body.String())
	}
	if got := h.cfg.APIKeys[0].Limits; got == nil || got.MaxRequests != 10 {
		t.Fatalf("PUT limits = %#v, want max-requests=10", got)
	}
	if got := h.cfg.APIKeys[1].Limits; got != nil {
		t.Fatalf("new key limits = %#v, want nil", got)
	}

	patchRecorder := httptest.NewRecorder()
	patchCtx, _ := gin.CreateTestContext(patchRecorder)
	patchCtx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/api-keys", strings.NewReader(`{"index":0,"value":"limited"}`))
	h.PatchAPIKeys(patchCtx)
	if patchRecorder.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want %d; body=%s", patchRecorder.Code, http.StatusOK, patchRecorder.Body.String())
	}
	if got := h.cfg.APIKeys[0].Limits; got == nil || got.MaxRequests != 10 {
		t.Fatalf("PATCH limits = %#v, want max-requests=10", got)
	}

	deleteRecorder := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRecorder)
	deleteCtx.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/api-keys?index=1", nil)
	deleteCtx.Request.Header.Set("If-Match", apiKeyRevision(h.cfg.APIKeys))
	h.DeleteAPIKeys(deleteCtx)
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want %d; body=%s", deleteRecorder.Code, http.StatusOK, deleteRecorder.Body.String())
	}
	if got := h.cfg.APIKeys; len(got) != 1 || got[0].Key != "limited" {
		t.Fatalf("remaining API keys = %#v, want only limited", got)
	}
}

func TestDeleteAPIKeysRequiresIndexAndRevision(t *testing.T) {
	newHandler := func() *Handler {
		return &Handler{
			cfg: &config.Config{SDKConfig: config.SDKConfig{
				APIKeys: []config.APIKeyEntry{{Key: "raw-key-must-stay-out-of-url"}},
			}},
			configFilePath: writeTestConfigFile(t),
		}
	}

	t.Run("missing revision", func(t *testing.T) {
		h := newHandler()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/api-keys?index=0", nil)
		h.DeleteAPIKeys(ctx)
		if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "config_revision_required") {
			t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
		}
		if len(h.cfg.APIKeys) != 1 {
			t.Fatal("missing-revision delete mutated keys")
		}
	})

	t.Run("raw value compatibility removed", func(t *testing.T) {
		h := newHandler()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/api-keys?value=legacy-selector", nil)
		ctx.Request.Header.Set("If-Match", apiKeyRevision(h.cfg.APIKeys))
		h.DeleteAPIKeys(ctx)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid index") {
			t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
		}
		if len(h.cfg.APIKeys) != 1 {
			t.Fatal("raw-value delete mutated keys")
		}
	})
}
