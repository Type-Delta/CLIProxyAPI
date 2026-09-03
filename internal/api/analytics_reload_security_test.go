package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	managementHandlers "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"gopkg.in/yaml.v3"
)

func TestConfigReloadRejectsBeforePublishWhenViewerRevocationCannotPersist(t *testing.T) {
	directory := t.TempDir()
	stateDirectory := filepath.Join(directory, "analytics")
	viewerPath := filepath.Join(stateDirectory, "viewers.json")
	store, err := managementHandlers.NewAnalyticsViewerStore(viewerPath)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(managementHandlers.ViewerCreateRequest{
		KeyID: strings.Repeat("a", 64), AllowedViews: []string{"summary"}, ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.Exchange(created.Credential)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(stateDirectory, stateDirectory+"-saved"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(stateDirectory, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldConfig := &config.Config{}
	oldYAML, err := yaml.Marshal(oldConfig)
	if err != nil {
		t.Fatal(err)
	}
	management := managementHandlers.NewHandlerWithoutConfigFilePath(oldConfig, nil)
	management.SetAnalyticsViewerStore(store)
	server := &Server{cfg: oldConfig, mgmt: management, oldConfigYaml: oldYAML}
	if server.UpdateClientsContext(context.Background(), &config.Config{Debug: true}) {
		t.Fatal("reload was published despite failed durable viewer revocation")
	}
	if server.cfg != oldConfig {
		t.Fatal("server configuration changed after rejected reload")
	}
	if _, err = store.Authenticate(token, "summary"); err != nil {
		t.Fatalf("rejected reload changed the prior session scope: %v", err)
	}
}

func TestSuccessfulReloadPreparationInvalidatesSessionsAndMarksViewerRestart(t *testing.T) {
	store, err := managementHandlers.NewAnalyticsViewerStore("")
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(managementHandlers.ViewerCreateRequest{
		KeyID: strings.Repeat("b", 64), AllowedViews: []string{"summary"}, ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.Exchange(created.Credential)
	if err != nil {
		t.Fatal(err)
	}
	management := managementHandlers.NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	management.SetAnalyticsViewerStore(store)
	if err = management.InvalidateAnalyticsViewerSessions(); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Authenticate(token, "summary"); !errors.Is(err, managementHandlers.ErrViewerSessionInvalid) {
		t.Fatalf("session survived successful reload preparation: %v", err)
	}

	service := cpauk.NewUnavailable("startup", cpauk.DefaultConfig())
	oldConfig := &config.Config{}
	newConfig := &config.Config{}
	newConfig.Analytics.Viewer.TrustedProxyCIDRs = []string{"172.30.0.0/24"}
	fields := markAnalyticsViewerRestartRequired(service, oldConfig, newConfig)
	if len(fields) != 3 || service.Health().Category != "restart_required" {
		t.Fatalf("restart marker fields=%v health=%+v", fields, service.Health())
	}
}
