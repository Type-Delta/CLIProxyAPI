package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
)

func TestUnknownManagementRoutesDiscoverAgentDocs(t *testing.T) {
	for _, tc := range []struct {
		name, path                                                   string
		enabled, home, missingHost, missingManagement, authenticated bool
		status                                                       int
		hint                                                         bool
	}{
		{name: "v0 authorized", path: "/v0/management/not-a-route", enabled: true, authenticated: true, status: 404, hint: true},
		{name: "v0 unauthorized", path: "/v0/management/not-a-route", enabled: true, status: 401},
		{name: "v8 public hint", path: "/v8/management/not-a-route", enabled: true, status: 404, hint: true},
		{name: "v8 disabled", path: "/v8/management/not-a-route", status: 404},
		{name: "home", path: "/v8/management/not-a-route", enabled: true, home: true, status: 404},
		{name: "v0 home", path: "/v0/management/not-a-route", enabled: true, home: true, status: 404},
		{name: "missing host", path: "/v8/management/not-a-route", enabled: true, missingHost: true, status: 404},
		{name: "missing management", path: "/v8/management/not-a-route", enabled: true, missingManagement: true, status: 404},
		{name: "unrelated", path: "/v8/management-other", enabled: true, status: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{RemoteManagement: config.RemoteManagement{SecretKey: "test-key"}}
			cfg.Home.Enabled = tc.home
			h := management.NewHandler(cfg, "", nil)
			h.SetLocalPassword("test-key")
			s := &Server{cfg: cfg, engine: gin.New(), mgmt: h, pluginHost: pluginhost.New()}
			s.managementRoutesEnabled.Store(tc.enabled)
			if tc.missingHost {
				s.pluginHost = nil
			}
			if tc.missingManagement {
				s.mgmt = nil
			}
			s.engine.NoRoute(s.pluginManagementNoRoute)
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.RemoteAddr = "127.0.0.1:1234"
			if tc.authenticated {
				req.Header.Set("Authorization", "Bearer test-key")
			}
			w := httptest.NewRecorder()
			s.engine.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			if tc.hint {
				if w.Body.String() != `{"agent_docs":"/llms.txt","error":"unknown management route; see /llms.txt for the management API"}` || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
					t.Fatalf("missing discovery hint: %s", w.Body)
				}
			} else if tc.status == 404 && w.Body.Len() != 0 {
				t.Fatalf("availability 404 got body: %s", w.Body)
			}
		})
	}
}
