package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"golang.org/x/crypto/bcrypt"
)

func TestEveryAnalyticsManagementRouteRequiresManagementAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "management-test-secret"
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{RemoteManagement: config.RemoteManagement{AllowRemote: true, SecretKey: string(hash)}}
	server := &Server{engine: gin.New(), cfg: cfg, mgmt: managementHandlers.NewHandlerWithoutConfigFilePath(cfg, nil)}
	server.managementRoutesEnabled.Store(true)
	server.registerManagementRoutes()

	analyticsRoutes := 0
	for index, route := range server.engine.Routes() {
		if !strings.HasPrefix(route.Path, "/v0/management/analytics/") {
			continue
		}
		analyticsRoutes++
		path := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(route.Path,
			":attempt_id", "attempt"), ":batch_id", "batch"), ":job_id", "job")
		path = strings.ReplaceAll(path, ":id", "resource")
		request := httptest.NewRequest(route.Method, path, nil)
		request.RemoteAddr = fmt.Sprintf("198.51.100.%d:4317", index+1)
		response := httptest.NewRecorder()
		server.engine.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without credentials status=%d body=%s", route.Method, route.Path, response.Code, response.Body.String())
		}
	}
	if analyticsRoutes < 22 {
		t.Fatalf("analytics management route count=%d, want at least 22", analyticsRoutes)
	}

	request := httptest.NewRequest(http.MethodGet, "/v0/management/capabilities", nil)
	request.RemoteAddr = "198.51.100.250:4317"
	request.Header.Set("Authorization", "Bearer "+secret)
	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("valid management credential status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEveryAnalyticsManagementRouteRejectsKeyIdentityInURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "management-url-key-secret"
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{RemoteManagement: config.RemoteManagement{AllowRemote: true, SecretKey: string(hash)}}
	server := &Server{engine: gin.New(), cfg: cfg, mgmt: managementHandlers.NewHandlerWithoutConfigFilePath(cfg, nil)}
	server.managementRoutesEnabled.Store(true)
	server.registerManagementRoutes()

	request := httptest.NewRequest(http.MethodGet, "/v0/management/analytics/health?key_id="+strings.Repeat("a", 64), nil)
	request.RemoteAddr = "198.51.100.240:4317"
	request.Header.Set("Authorization", "Bearer "+secret)
	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "analytics_invalid_query") {
		t.Fatalf("key identity URL status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestManagementAndViewerCredentialsCannotCrossGroups(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "management-cross-group-secret"
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{RemoteManagement: config.RemoteManagement{AllowRemote: true, SecretKey: string(hash)}}
	store, err := managementHandlers.NewAnalyticsViewerStore("")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := store.Create(managementHandlers.ViewerCreateRequest{
		KeyID: strings.Repeat("a", 64), AllowedViews: []string{"summary"}, ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	management := managementHandlers.NewHandlerWithoutConfigFilePath(cfg, nil)
	server := &Server{
		engine: engine, cfg: cfg, mgmt: management,
		analytics: &viewerRouteService{reader: &viewerRouteReader{}},
	}
	server.managementRoutesEnabled.Store(true)
	server.registerManagementRoutes()
	if err = server.RegisterAnalyticsViewerRoutes(store, AnalyticsViewerSecurityOptions{}); err != nil {
		t.Fatal(err)
	}

	managementRequest := httptest.NewRequest(http.MethodGet, "/v0/management/capabilities", nil)
	managementRequest.RemoteAddr = "198.51.100.8:4317"
	managementRequest.Header.Set("Authorization", "Bearer "+viewer.Credential)
	managementResponse := httptest.NewRecorder()
	engine.ServeHTTP(managementResponse, managementRequest)
	if managementResponse.Code != http.StatusUnauthorized {
		t.Fatalf("viewer credential reached management group: %d", managementResponse.Code)
	}

	viewerRequest := httptest.NewRequest(http.MethodGet,
		"https://cpa.test/v0/analytics/viewer/summary?start=2026-08-01T00:00:00Z&end=2026-09-01T00:00:00Z&time_zone=UTC", nil)
	viewerRequest.Header.Set("Authorization", "Bearer "+secret)
	viewerResponse := httptest.NewRecorder()
	engine.ServeHTTP(viewerResponse, viewerRequest)
	if viewerResponse.Code != http.StatusUnauthorized {
		t.Fatalf("management credential reached viewer group: %d", viewerResponse.Code)
	}
}
