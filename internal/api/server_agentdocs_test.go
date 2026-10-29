package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentdocs"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestAgentDocsRouteParity(t *testing.T) {
	s := newTestServerWithConfig(t, &config.Config{RemoteManagement: config.RemoteManagement{SecretKey: "test-key"}})
	s.clickHouse.handle = chtest.Handle(t)
	s.startTelemetry(t.TempDir())
	t.Cleanup(func() {
		s.stopTelemetry()
		s.clickHouse.handle = nil // chtest owns database cleanup and connection closure.
		if err := s.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	content, err := agentdocs.Files.ReadFile("management.md")
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	// Inventory rows contain exactly one `METHOD /v8/management/path` span.
	pattern := regexp.MustCompile("(?m)^\\| `(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS) (/v8/management[^` ]*)` \\|")
	for _, match := range pattern.FindAllStringSubmatch(string(content), -1) {
		key := match[1] + " " + match[2]
		if documented[key] {
			t.Errorf("duplicate inventory row: %s", key)
		}
		documented[key] = true
	}
	registered := map[string]bool{}
	for _, route := range s.engine.Routes() {
		if strings.HasPrefix(route.Path, "/v8/management/") {
			key := route.Method + " " + route.Path
			registered[key] = true
			if !documented[key] {
				t.Errorf("undocumented route: %s", key)
			}
		}
	}
	for key := range documented {
		if !registered[key] {
			t.Errorf("documented route is not registered: %s", key)
		}
	}
	if len(registered) == 0 || len(documented) == 0 {
		t.Fatal("empty route inventory")
	}
	t.Logf("matched %d management routes", len(registered))
}

func TestAgentDocsServing(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	for _, tc := range []struct {
		name                               string
		enabled, home, safe, panelDisabled bool
	}{
		{name: "enabled", enabled: true}, {name: "no management key"},
		{name: "home", enabled: true, home: true}, {name: "safe mode", enabled: true, safe: true},
		{name: "panel disabled", enabled: true, panelDisabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Home.Enabled = tc.home
			cfg.RemoteManagement.DisableControlPanel = tc.panelDisabled
			var opts []ServerOption
			if tc.enabled {
				cfg.RemoteManagement.SecretKey = "test-secret"
				opts = append(opts, WithLocalManagementPassword("test-secret"))
			}
			if tc.safe {
				cfg.APIKeys = []config.APIKeyEntry{{Key: "your-api-key-1"}}
				opts = append(opts, WithExampleAPIKeySafeMode())
			}
			s := newTestServerWithConfig(t, cfg, opts...)
			t.Cleanup(func() {
				if err := s.Stop(context.Background()); err != nil {
					t.Error(err)
				}
			})
			available := tc.enabled && !tc.home
			request := func(method, path, remote string) *httptest.ResponseRecorder {
				t.Helper()
				r := httptest.NewRequest(method, path, nil)
				r.RemoteAddr = remote
				w := httptest.NewRecorder()
				s.engine.ServeHTTP(w, r)
				return w
			}
			for _, path := range []string{"/llms.txt", "/llms/management.md", "/llms/sql.md"} {
				for _, method := range []string{http.MethodGet, http.MethodHead} {
					w := request(method, path, "198.51.100.2:1234")
					want := http.StatusNotFound
					if available {
						want = http.StatusOK
					}
					if w.Code != want {
						t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
					}
					if !available {
						if w.Header().Get("Link") != "" {
							t.Fatal("unavailable docs advertised")
						}
						continue
					}
					ct := "text/markdown; charset=utf-8"
					if path == "/llms.txt" {
						ct = "text/plain; charset=utf-8"
					}
					if w.Header().Get("Content-Type") != ct {
						t.Errorf("content type: %s", w.Header().Get("Content-Type"))
					}
					name := strings.TrimPrefix(path, "/llms/")
					if path == "/llms.txt" {
						name = "llms.txt"
					}
					data, err := agentdocs.Files.ReadFile(name)
					if err != nil {
						t.Fatal(err)
					}
					if method == http.MethodHead && w.Body.Len() != 0 {
						t.Fatal("HEAD returned a body")
					}
					if method == http.MethodGet && w.Body.String() != string(data) {
						t.Fatal("embedded body mismatch")
					}
				}
			}
			for _, remote := range []string{"127.0.0.1:1234", "198.51.100.2:1234"} {
				w := request(http.MethodGet, "/v8/management/config", remote)
				want := 404
				if available {
					want = 401
					if strings.HasPrefix(remote, "198.") {
						want = 403
					}
				}
				if w.Code != want {
					t.Fatalf("v8 status %d want %d: %s", w.Code, want, w.Body)
				}
				if got := w.Header().Get("Link"); (got == agentDocsLink) != available {
					t.Errorf("v8 discovery=%q available=%v", got, available)
				}
			}
			w := request(http.MethodGet, "/", "127.0.0.1:1234")
			if got := w.Header().Get("Link"); (got == agentDocsLink) != available {
				t.Errorf("root discovery=%q available=%v", got, available)
			}
			if !tc.home && !tc.safe {
				var root struct {
					Message   string
					Endpoints []string
					AgentDocs string `json:"agent_docs"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &root); err != nil {
					t.Fatal(err)
				}
				if root.Message != "CLI Proxy API Server" || len(root.Endpoints) != 3 {
					t.Fatal("root fields changed")
				}
				if (root.AgentDocs == "/llms.txt") != available {
					t.Fatalf("agent_docs=%q", root.AgentDocs)
				}
			}
			// Gate follows live state even after routes have been registered.
			s.managementRoutesEnabled.Store(false)
			if w := request(http.MethodGet, "/llms.txt", "127.0.0.1:1234"); w.Code != 404 || w.Header().Get("Link") != "" {
				t.Fatal("disabled docs remained available")
			}
		})
	}
}
