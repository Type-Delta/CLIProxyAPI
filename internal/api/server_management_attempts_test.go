package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"golang.org/x/crypto/bcrypt"
)

type managementAttemptService struct {
	*viewerRouteService
	event model.Event
	query model.Query
}

func (s *managementAttemptService) EventByAttemptID(_ context.Context, id string, query model.Query) (model.Event, bool, error) {
	s.query = query
	return s.event, id == s.event.AttemptID, nil
}

func TestManagementAttemptDetailAlias(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	owner, err := bcrypt.GenerateFromPassword([]byte("owner-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := bcrypt.GenerateFromPassword([]byte("viewer-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{RemoteManagement: config.RemoteManagement{SecretKey: string(owner), Keys: []config.ManagementKey{{ID: "viewer", Name: "Viewer", SecretKey: string(viewer), Scope: config.ManagementScopeReadOnly}}}}
	s := newTestServerWithConfig(t, cfg)
	service := &managementAttemptService{viewerRouteService: &viewerRouteService{reader: &viewerRouteReader{}}, event: model.Event{SchemaVersion: 1, AttemptID: strings.Repeat("a", 32), Model: "demo-model", RequestedAt: time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC), Succeeded: true}}
	s.mgmt.SetAnalyticsService(service)
	for _, resource := range []string{"attempts", "events"} {
		t.Run(resource, func(t *testing.T) {
			path := "/v8/management/analytics/" + resource + "/" + service.event.AttemptID + "?start=2026-11-07T00:00:00Z&end=2026-11-08T00:00:00Z&time_zone=UTC"
			for _, credential := range []string{"", "owner-secret", "viewer-secret"} {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.RemoteAddr = "127.0.0.1:1234"
				if credential != "" {
					req.Header.Set("Authorization", "Bearer "+credential)
				}
				res := httptest.NewRecorder()
				s.engine.ServeHTTP(res, req)
				if credential == "" {
					if res.Code != http.StatusUnauthorized {
						t.Fatalf("unauthenticated status=%d", res.Code)
					}
					continue
				}
				if res.Code != http.StatusOK {
					t.Fatalf("detail status=%d body=%s", res.Code, res.Body.String())
				}
				var event model.Event
				if err := json.Unmarshal(res.Body.Bytes(), &event); err != nil {
					t.Fatal(err)
				}
				if event.AttemptID != service.event.AttemptID || event.Model != service.event.Model {
					t.Fatalf("unexpected detail: %+v", event)
				}
				if !strings.Contains(res.Header().Get("Cache-Control"), "no-store") {
					t.Fatalf("detail cache-control=%q", res.Header().Get("Cache-Control"))
				}
				if !service.query.Start.Equal(time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)) || service.query.TimeZone != "UTC" {
					t.Fatalf("detail range not forwarded: %+v", service.query)
				}
			}
			req := httptest.NewRequest(http.MethodGet, path+"&key_id="+strings.Repeat("b", 64), nil)
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set("Authorization", "Bearer viewer-secret")
			res := httptest.NewRecorder()
			s.engine.ServeHTTP(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("key ID in URL status=%d body=%s", res.Code, res.Body.String())
			}
		})
	}
}
