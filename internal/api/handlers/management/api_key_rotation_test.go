package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// TestPatchAPIKeysRotationPreservesLimits exercises both PATCH routes end to
// end, so the regression is caught even if the handler stops using the helper.
func TestPatchAPIKeysRotationPreservesLimits(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "old/new form", body: `{"old":"old-key","new":"new-key"}`},
		{name: "index/value form", body: `{"index":0,"value":"new-key"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handler{
				cfg: &config.Config{SDKConfig: config.SDKConfig{
					APIKeys: []config.APIKeyEntry{{
						Key:    "old-key",
						Limits: &config.KeyLimits{MaxRequests: 7, MaxTokensM: 0.0009},
					}},
				}},
				configFilePath: writeTestConfigFile(t),
			}

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/api-keys", strings.NewReader(tt.body))
			h.PatchAPIKeys(ctx)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
			}
			if got := h.cfg.APIKeys[0].Key; got != "new-key" {
				t.Fatalf("Key = %q, want new-key", got)
			}
			limits := h.cfg.APIKeys[0].Limits
			if limits == nil {
				t.Fatal("rotating the key dropped its limits")
			}
			if limits.MaxRequests != 7 || limits.MaxTokensM != 0.0009 {
				t.Fatalf("limits = %+v, want MaxRequests=7 MaxTokensM=0.0009", *limits)
			}
		})
	}
}

// TestAPIKeyEntryReplacingPreservesLimits covers key rotation through PATCH.
// The limits belong to the slot being replaced, so a rotated key must keep them
// rather than silently becoming unlimited.
func TestAPIKeyEntryReplacingPreservesLimits(t *testing.T) {
	previous := config.APIKeyEntry{
		Key:    "old-key",
		Limits: &config.KeyLimits{MaxRequests: 5, MaxTokensM: 0.001},
	}

	entry := apiKeyEntryReplacing("new-key", previous)

	if entry.Key != "new-key" {
		t.Fatalf("Key = %q, want new-key", entry.Key)
	}
	if entry.Limits == nil {
		t.Fatal("limits were dropped when the key was rotated")
	}
	if entry.Limits.MaxRequests != 5 || entry.Limits.MaxTokensM != 0.001 {
		t.Fatalf("limits = %+v, want MaxRequests=5 MaxTokensM=0.001", *entry.Limits)
	}

	// The copy must be independent of the original entry.
	entry.Limits.MaxRequests = 99
	if previous.Limits.MaxRequests != 5 {
		t.Fatal("limits are shared with the previous entry instead of copied")
	}
}

// TestAPIKeyEntryReplacingWithoutLimits keeps unlimited keys unlimited.
func TestAPIKeyEntryReplacingWithoutLimits(t *testing.T) {
	entry := apiKeyEntryReplacing("new-key", config.APIKeyEntry{Key: "old-key"})
	if entry.Limits != nil {
		t.Fatalf("Limits = %+v, want nil", *entry.Limits)
	}
}
