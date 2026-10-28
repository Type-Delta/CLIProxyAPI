package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"golang.org/x/crypto/bcrypt"
)

func TestQuotaHistoryRequiresManagementAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	hash, err := bcrypt.GenerateFromPassword([]byte("test-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{RemoteManagement: config.RemoteManagement{AllowRemote: true, SecretKey: string(hash)}}
	server := &Server{engine: gin.New(), cfg: cfg, mgmt: managementHandlers.NewHandlerWithoutConfigFilePath(cfg, nil)}
	server.managementRoutesEnabled.Store(true)
	server.registerManagementRoutes()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		request := httptest.NewRequest(method, "/v8/management/analytics/quota-history", nil)
		request.RemoteAddr = "198.51.100.2:1234"
		response := httptest.NewRecorder()
		server.engine.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s status=%d", method, response.Code)
		}
	}
}
