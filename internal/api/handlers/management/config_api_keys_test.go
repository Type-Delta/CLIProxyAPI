package management

import (
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

	if got, want := recorder.Body.String(), `{"api-keys":["first","second"]}`; got != want {
		t.Fatalf("response = %s, want %s", got, want)
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
	if putRecorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want %d; body=%s", putRecorder.Code, http.StatusOK, putRecorder.Body.String())
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
	deleteCtx.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/api-keys?value=new", nil)
	h.DeleteAPIKeys(deleteCtx)
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want %d; body=%s", deleteRecorder.Code, http.StatusOK, deleteRecorder.Body.String())
	}
	if got := h.cfg.APIKeys; len(got) != 1 || got[0].Key != "limited" {
		t.Fatalf("remaining API keys = %#v, want only limited", got)
	}
}
