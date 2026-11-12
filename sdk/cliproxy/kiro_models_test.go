package cliproxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type kiroCatalogTransport func(*http.Request) (*http.Response, error)

func (f kiroCatalogTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestKiroRegistersOnlyDiscoveredModels(t *testing.T) {
	var calls atomic.Int32
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", kiroCatalogTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Host != "codewhisperer.eu-west-1.amazonaws.com" || r.URL.Path != "/ListAvailableModels" || r.URL.Query().Get("origin") != "AI_EDITOR" || r.URL.Query().Get("profileArn") != "test-profile" {
			t.Errorf("unexpected catalog request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("catalog request did not use the credential token")
		}
		body := `{"models":[{"modelId":"claude-fable-5.1","modelName":"Claude Fable 5.1","tokenLimits":{"maxInputTokens":123000,"maxOutputTokens":8192}}],"nextToken":"page-2"}`
		if r.URL.Query().Get("nextToken") == "page-2" {
			body = `{"models":[{"modelId":"claude-fable-7.12","modelName":"Future Claude"},{"modelId":"qwen3-coder-next","modelName":"Qwen"},{"modelId":"auto","modelName":"Auto"}]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	}))
	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{ID: t.Name(), Provider: "kiro", Metadata: map[string]any{
		"access_token": "test-token", "region": "eu-west-1", "profile_arn": "test-profile",
	}}
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	service.registerModelsForAuth(ctx, auth)
	models := reg.GetModelsForClient(auth.ID)
	if len(models) != 4 {
		t.Fatalf("registered %d models, want the four discovered Kiro models", len(models))
	}
	for _, id := range []string{"claude-fable-5-1", "claude-fable-7-12", "qwen3-coder-next", "auto"} {
		if !reg.ClientSupportsModel(auth.ID, id) {
			t.Errorf("discovered model %q was not registered", id)
		}
	}
	for _, id := range []string{"claude-fable-5-2", "claude-fable-5", "claude-opus-5-5"} {
		if reg.ClientSupportsModel(auth.ID, id) {
			t.Errorf("undiscovered model %q remains eligible for Kiro routing", id)
		}
	}
	for _, model := range models {
		if model.Type != "claude" {
			t.Errorf("model %q type = %q, want Claude-compatible translation", model.ID, model.Type)
		}
		if model.ID == "claude-fable-5-1" && (model.ContextLength != 123000 || model.MaxCompletionTokens != 8192 || model.DisplayName != "Claude Fable 5.1") {
			t.Errorf("discovered metadata was not retained: %+v", model)
		}
	}
	service.registerModelsForAuth(ctx, auth)
	if calls.Load() != 2 {
		t.Errorf("catalog calls = %d, want two pages shared across registrations", calls.Load())
	}
}

func TestKiroCatalogFailureDoesNotAdvertiseClaudeModels(t *testing.T) {
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", kiroCatalogTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header), Request: r}, nil
	}))
	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{ID: t.Name(), Provider: "kiro", Metadata: map[string]any{"access_token": "test-token"}}
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	service.registerModelsForAuth(ctx, auth)
	if models := reg.GetModelsForClient(auth.ID); len(models) != 0 {
		t.Fatalf("failed discovery advertised %d inferred models", len(models))
	}
}

func TestKiroRefreshIsolatesCredentialsAndPreservesModelSettings(t *testing.T) {
	var calls atomic.Int32
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", kiroCatalogTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		body := `{"models":[{"modelId":"claude-fable-5.1"},{"modelId":"claude-opus-5.5"}]}`
		if r.Header.Get("Authorization") == "Bearer second" {
			body = `{"models":[{"modelId":"claude-sonnet-5.5"}]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	}))
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{coreManager: manager, cfg: &config.Config{
		SDKConfig:           config.SDKConfig{ForceModelPrefix: true},
		OAuthExcludedModels: map[string][]string{"kiro": {"claude-opus-5-5"}},
		OAuthModelAlias:     map[string][]config.OAuthModelAlias{"claude": {{Name: "claude-fable-5-1", Alias: "fable"}}},
	}}
	reg := GlobalModelRegistry()
	for _, token := range []string{"first", "second"} {
		auth, errRegister := manager.Register(ctx, &coreauth.Auth{ID: t.Name() + token, Provider: "kiro", Prefix: token, Metadata: map[string]any{"access_token": token}})
		if errRegister != nil {
			t.Fatal(errRegister)
		}
		t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	}
	service.refreshKiroModels(ctx)
	firstID, secondID := t.Name()+"first", t.Name()+"second"
	if !reg.ClientSupportsModel(firstID, "first/fable") || reg.ClientSupportsModel(firstID, "first/claude-opus-5-5") || reg.ClientSupportsModel(firstID, "first/claude-sonnet-5-5") {
		t.Fatalf("first credential lost its alias/prefix/exclusion or inherited another account's catalog: %+v", reg.GetModelsForClient(firstID))
	}
	if !reg.ClientSupportsModel(secondID, "second/claude-sonnet-5-5") || reg.ClientSupportsModel(secondID, "second/fable") {
		t.Fatal("second credential inherited the first account's catalog")
	}
	epoch := reg.ClientRegistrationEpoch(firstID)
	service.refreshKiroModels(ctx)
	if calls.Load() != 2 || reg.ClientRegistrationEpoch(firstID) != epoch {
		t.Fatal("periodic scan republished fresh catalogs")
	}
}

func TestKiroDiscoveryCannotResurrectRemovedCredential(t *testing.T) {
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", kiroCatalogTransport(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"models":[{"modelId":"claude-fable-5.1"}]}`)), Header: make(http.Header), Request: r}, nil
	}))
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{coreManager: manager, cfg: &config.Config{}}
	auth, errRegister := manager.Register(ctx, &coreauth.Auth{ID: t.Name(), Provider: "kiro", Metadata: map[string]any{"access_token": "token"}})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	reg := GlobalModelRegistry()
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	go func() {
		defer close(done)
		service.completeModelRegistrationForAuth(ctx, auth)
	}()
	<-entered
	service.applyCoreAuthRemoval(ctx, auth.ID)
	close(release)
	<-done
	if len(reg.GetModelsForClient(auth.ID)) != 0 {
		t.Fatal("late catalog response resurrected a removed credential")
	}
}
