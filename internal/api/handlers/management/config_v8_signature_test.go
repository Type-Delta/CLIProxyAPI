package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestConfigV8SignatureLegacyReadToggleSaveReload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "server: {port:8317}\nupstream:\n  claude:\n    header-defaults:\n      stabilize-device-profile: true\n      future-option: preserve\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.GET("/v8/management/config.yaml", handler.ConfigV8)
	router.PATCH("/v8/management/config", handler.ConfigV8)
	read := httptest.NewRecorder()
	router.ServeHTTP(read, httptest.NewRequest(http.MethodGet, "/v8/management/config.yaml", nil))
	if read.Code != 200 || !strings.Contains(read.Body.String(), "signature-stabilization: true") || strings.Contains(read.Body.String(), "stabilize-device-profile:") {
		t.Fatalf("legacy GET not canonical: %d %s", read.Code, read.Body.String())
	}
	for _, value := range []string{"false", "true"} {
		write := httptest.NewRecorder()
		router.ServeHTTP(write, httptest.NewRequest(http.MethodPatch, "/v8/management/config", strings.NewReader(`{"upstream":{"claude":{"header-defaults":{"signature-stabilization":`+value+`}}}}`)))
		if write.Code != 200 {
			t.Fatalf("toggle save: %d %s", write.Code, write.Body.String())
		}
		loaded, err := config.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.ClaudeHeaderDefaults.StabilizeDeviceProfile == nil || *loaded.ClaudeHeaderDefaults.StabilizeDeviceProfile != (value == "true") {
			t.Fatalf("reload lost toggle %s", value)
		}
		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(saved), "stabilize-device-profile:") || !strings.Contains(string(saved), "future-option:") {
			t.Fatalf("save lost unknown field or legacy key survived: %s", saved)
		}
	}
}
