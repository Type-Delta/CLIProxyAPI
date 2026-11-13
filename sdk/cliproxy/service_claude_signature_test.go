package cliproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestClaudeSignatureConfigEnableMigratesBeforeAliasRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.db")
	if err := kvstore.ConfigureLocal(true, path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kvstore.ShutdownLocal() })
	client, active, err := kvstore.CurrentKVClient()
	if err != nil || !active {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"user_agent": "claude-cli/2.1.322 (external, sdk-ts)", "package_version": "0.128.0", "runtime_version": "v26.12.1", "os": "MacOS", "arch": "arm64", "device_id": strings.Repeat("b", 64)})
	if _, err := client.KVSet(t.Context(), "cpa:claude:device-profile:"+kvstore.HashKeyPart("auth:z-original.json"), raw, kvstore.KVSetOptions{}); err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	var copy *coreauth.Auth
	for _, id := range []string{"a-copy.json", "z-original.json"} {
		a := &coreauth.Auth{ID: id, Provider: "claude", Status: coreauth.StatusActive, Metadata: map[string]any{"account_uuid": "account-a", "organization_uuid": "org-a", "claude_device_ids": []string{strings.Repeat("b", 64)}}}
		registered, err := manager.Register(coreauth.WithSkipPersist(t.Context()), a)
		if err != nil {
			t.Fatal(err)
		}
		if id == "a-copy.json" {
			copy = registered
		}
	}
	off := false
	on := true
	cfgOff := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &off}}
	service := &Service{cfg: cfgOff, coreManager: manager}
	cfgOn := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &on}}
	if !service.applyConfigUpdateWithAuthSynthesis(context.Background(), cfgOn, false) {
		t.Fatal("signature enable config rejected")
	}
	if !helps.ClaudeSignatureStabilizationEnabled(service.currentConfig()) {
		t.Fatal("enabled config not published")
	}
	headers := http.Header{"User-Agent": {"claude-cli/2.1.318 (external, sdk-ts)"}, "X-Stainless-Package-Version": {"0.128.0"}, "X-Stainless-Runtime-Version": {"v26.12.1"}}
	profile, err := helps.ResolveClaudeDeviceProfileRequired(t.Context(), copy, "", headers, cfgOn)
	if err != nil {
		t.Fatal(err)
	}
	if profile.UserAgent != "claude-cli/2.1.322 (external, sdk-ts)" {
		t.Fatalf("enable exposed canonical state below known legacy: %+v", profile)
	}
}
