package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type credentialEventService struct{ *analyticsHandlerService }

func (s *credentialEventService) CredentialID(provider, index, id string) (*string, error) {
	return model.CredentialID([]byte(strings.Repeat("k", 32)), provider, index, id)
}

func TestAnalyticsEventDetailResolvesCredentialFilename(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name, filename, provider string
		want                     *string
	}{
		{"filename", "/private/auths/codex-test.json", "codex", new("codex-test.json")},
		{"windows filename", `C:\private\auths\codex-test.json`, "codex", new("codex-test.json")},
		{"no backing file", "", "codex", nil},
		{"different provider", "other.json", "claude", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: "credential-test", Provider: test.provider, FileName: test.filename})
			if err != nil {
				t.Fatal(err)
			}
			service := &credentialEventService{&analyticsHandlerService{reader: &analyticsHandlerReader{}, state: model.StateReady}}
			id, err := service.CredentialID("codex", auth.Index, auth.ID)
			if err != nil {
				t.Fatal(err)
			}
			event := model.Event{AttemptID: strings.Repeat("a", 32), Provider: "codex", CredentialID: id}
			service.reader.events.Events = []model.Event{event}
			handler := &Handler{analytics: service, authManager: manager}
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Params = gin.Params{{Key: "attempt_id", Value: event.AttemptID}}
			ctx.Request = httptest.NewRequest(http.MethodGet, "/?start=2026-08-01T00:00:00Z&end=2026-09-01T00:00:00Z&time_zone=UTC", nil)
			handler.GetAnalyticsEvent(ctx)
			if response.Code != http.StatusOK {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			var got struct {
				Filename *string `json:"credential_filename"`
				ID       *string `json:"credential_id"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if (got.Filename == nil) != (test.want == nil) || (got.Filename != nil && *got.Filename != *test.want) {
				t.Fatalf("filename = %v, want %v", got.Filename, test.want)
			}
			if got.ID == nil || *got.ID != *id {
				t.Fatal("hashed identity changed")
			}
		})
	}
}

func TestAnalyticsDimensionsResolveCredentialDisplayNames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "credential-dimension", Provider: "codex", FileName: "/private/auths/codex-test.json",
		Attributes: map[string]string{"label": "Team Codex"},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &credentialEventService{&analyticsHandlerService{reader: &analyticsHandlerReader{}, state: model.StateReady}}
	id, err := service.CredentialID("codex", auth.Index, auth.ID)
	if err != nil {
		t.Fatal(err)
	}
	service.reader.dimensions = model.DimensionPage{Rows: []model.DimensionRow{{Value: *id}}}
	for _, test := range []struct {
		name    string
		manager *coreauth.Manager
		want    bool
	}{
		{name: "admin", manager: manager, want: true},
		{name: "unloaded manager", manager: nil, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			service.reader.dimensions = model.DimensionPage{Rows: []model.DimensionRow{{Value: *id}}}
			handler := &Handler{analytics: service, authManager: test.manager}
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/?start=2026-08-01T00:00:00Z&end=2026-09-01T00:00:00Z&time_zone=UTC&dimension=credential", nil)
			handler.GetAnalyticsDimensions(ctx)
			if response.Code != http.StatusOK {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			var got struct {
				Rows []struct {
					Filename *string `json:"credential_filename"`
					Label    *string `json:"credential_label"`
				} `json:"rows"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Rows) != 1 {
				t.Fatalf("rows = %+v", got.Rows)
			}
			if !test.want {
				if got.Rows[0].Filename != nil || got.Rows[0].Label != nil {
					t.Fatalf("unnamed auth manager leaked display fields: %+v", got.Rows[0])
				}
				return
			}
			if got.Rows[0].Filename == nil || *got.Rows[0].Filename != "codex-test.json" || got.Rows[0].Label == nil || *got.Rows[0].Label != "Team Codex" {
				t.Fatalf("display fields = %+v", got.Rows[0])
			}
		})
	}
}
