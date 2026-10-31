package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestTelemetryRouteModes(t *testing.T) {
	for _, configured := range []bool{false, true} {
		name := "plain"
		if configured {
			name = "clickhouse"
		}
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{ClickHouse: config.DefaultClickHouseConfig()}
			// Install the isolated test database before management route registration.
			s := newTestServerWithConfig(t, cfg)
			cfg = s.getConfig().CloneForRuntime()
			if configured {
				s.clickHouse.handle = chtest.Handle(t)
				s.mgmt.SetClickHouse(s.clickHouse.handle)
				cfg.ClickHouse.DSN = "clickhouse://startup-test"
			} else if _, ok := s.requestLogger.(*logging.FileRequestLogger); !ok {
				t.Fatalf("plain logger=%T", s.requestLogger)
			}
			t.Cleanup(func() {
				s.stopTelemetry()
				if configured {
					s.clickHouse.handle = nil
				} // chtest owns the connection.
				if err := s.Stop(context.Background()); err != nil {
					t.Error(err)
				}
			})
			cfg.RemoteManagement.SecretKey = "test-key"
			s.cfgMu.Lock()
			s.cfg = cfg
			s.cfgMu.Unlock()
			s.mgmt.SetConfig(cfg)
			if configured {
				s.startTelemetry(t.TempDir())
				waitTelemetryCondition(t, func() bool {
					s.telemetry.mu.RLock()
					defer s.telemetry.mu.RUnlock()
					return s.telemetry.ingester != nil
				})
			}
			s.localPassword = "telemetry-test-key"
			s.mgmt.SetLocalPassword(s.localPassword)
			s.managementRoutesEnabled.Store(true)
			s.registerManagementRoutes()
			check := func() {
				t.Helper()
				for _, route := range []struct{ method, path, body string }{
					{http.MethodGet, "/v8/management/telemetry/schema", ""},
					{http.MethodPost, "/v8/management/telemetry/query", `{"sql":"SELECT 1"}`},
					{http.MethodGet, "/v8/management/telemetry/health", ""},
				} {
					req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
					req.RemoteAddr = "127.0.0.1:1234"
					req.Header.Set("Authorization", "Bearer telemetry-test-key")
					req.Header.Set("Content-Type", "application/json")
					w := httptest.NewRecorder()
					s.engine.ServeHTTP(w, req)
					want := http.StatusNotFound
					if configured {
						want = http.StatusOK
					}
					if w.Code != want {
						t.Errorf("%s %s status=%d want=%d body=%s", route.method, route.path, w.Code, want, w.Body.String())
					}
				}
			}
			check()
			// DSN changes require restart, so all three routes keep their startup mode.
			reloaded := s.getConfig().CloneForRuntime()
			if configured {
				reloaded.ClickHouse.DSN = ""
			} else {
				reloaded.ClickHouse.DSN = "clickhouse://localhost:9000"
			}
			s.UpdateClients(reloaded)
			check()
		})
	}
}
