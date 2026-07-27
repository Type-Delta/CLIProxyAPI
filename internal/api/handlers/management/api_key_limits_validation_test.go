package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// TestPutAPIKeysValidatesLimits covers structured limit entries arriving through
// the management API. Without validation an invalid cadence or an out-of-range
// cap is persisted to disk behind a 200 and only surfaces as a failure on the
// next config load, leaving runtime and on-disk config inconsistent.
func TestPutAPIKeysValidatesLimits(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "plain strings accepted",
			body:       `["alpha","beta"]`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "valid limits accepted",
			body:       `[{"key":"alpha","limits":{"max-requests":1000,"max-tokens-m":20,"resets":"weekly"}}]`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "unknown cadence rejected",
			body:       `[{"key":"alpha","limits":{"max-requests":10,"resets":"fortnightly"}}]`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "negative requests rejected",
			body:       `[{"key":"alpha","limits":{"max-requests":-10}}]`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "negative tokens rejected",
			body:       `[{"key":"alpha","limits":{"max-tokens-m":-1}}]`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "absurd token cap rejected",
			body:       `[{"key":"alpha","limits":{"max-tokens-m":1e19}}]`,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := []config.APIKeyEntry{{Key: "original"}}
			h := &Handler{
				cfg:            &config.Config{SDKConfig: config.SDKConfig{APIKeys: original}},
				configFilePath: writeTestConfigFile(t),
			}

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/api-keys", strings.NewReader(test.body))
			h.PutAPIKeys(ctx)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if test.wantStatus != http.StatusOK {
				if len(h.cfg.APIKeys) != 1 || h.cfg.APIKeys[0].Key != "original" {
					t.Fatalf("rejected payload still mutated config: %+v", h.cfg.APIKeys)
				}
			}
		})
	}
}
