package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestManagementCapabilitiesWithoutAnalyticsReportClickHouse(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/capabilities", nil)
	h.GetCapabilities(c)
	var result struct {
		Analytics struct {
			StorageDriver string `json:"storage_driver"`
		} `json:"analytics"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || result.Analytics.StorageDriver != "clickhouse" {
		t.Fatalf("capabilities=%s", w.Body)
	}
}
