package management

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type providerRowsDisplayTestService struct {
	*analyticsHandlerService
	credentials []model.ProviderCredential
}

func (s *providerRowsDisplayTestService) ProviderCredentials(context.Context) ([]model.ProviderCredential, error) {
	return s.credentials, nil
}

func (s *providerRowsDisplayTestService) CredentialID(provider, index, id string) (*string, error) {
	return model.CredentialID([]byte(strings.Repeat("k", 32)), provider, index, id)
}

func (s *providerRowsDisplayTestService) ReplaceProviderQuotaSnapshots(context.Context, []store.ProviderQuotaSnapshot) error {
	return nil
}

func (s *providerRowsDisplayTestService) ProviderQuotaSnapshots(context.Context) ([]store.ProviderQuotaSnapshot, error) {
	return nil, nil
}

var _ cpauk.Service = (*providerRowsDisplayTestService)(nil)

func TestAnalyticsProviderRowsPopulatesDisplayName(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "credential-id", Provider: "codex", FileName: "/auth/team-account.json",
		Attributes: map[string]string{"path": "/auth/team-account.json", "auth_kind": "oauth"},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &providerRowsDisplayTestService{analyticsHandlerService: &analyticsHandlerService{reader: &analyticsHandlerReader{}, state: model.StateReady}}
	credentialID, err := service.CredentialID(auth.Provider, auth.Index, auth.ID)
	if err != nil {
		t.Fatal(err)
	}
	service.credentials = []model.ProviderCredential{{Provider: "codex", CredentialID: *credentialID}}
	handler := &Handler{authManager: manager}
	rows, ok, err := handler.analyticsProviderRows(context.Background(), service)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || len(rows) != 1 {
		t.Fatalf("rows = %#v, available = %v", rows, ok)
	}
	if rows[0].DisplayName != "team-account.json" {
		t.Fatalf("display name = %q, want %q", rows[0].DisplayName, "team-account.json")
	}
}
