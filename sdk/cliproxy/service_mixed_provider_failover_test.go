package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type serviceMixedProviderExecutor struct {
	identifier string
	execute    func(auth *coreauth.Auth, req cliproxyexecutor.Request) (cliproxyexecutor.Response, error)
}

func (e *serviceMixedProviderExecutor) Identifier() string { return e.identifier }

func (e *serviceMixedProviderExecutor) Execute(_ context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.execute == nil {
		return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
	}
	return e.execute(auth, req)
}

func (e *serviceMixedProviderExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *serviceMixedProviderExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *serviceMixedProviderExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (e *serviceMixedProviderExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

// TestServiceMixedProvidersKeepModelAndFailOver builds the production layout through the real
// service model registration path: OpenCode Go as a Codex API key with custom models, and Z.ai
// as an OpenAI-compatible provider, both advertising glm-5.3-flash. Exhausting Z.ai must leave
// the catalog entry intact and route the next request to the Codex credential.
func TestServiceMixedProvidersKeepModelAndFailOver(t *testing.T) {
	const (
		model       = "glm-5.3-flash"
		codexAuthID = "service-mixed-codex"
		zaiAuthID   = "service-mixed-zai"
	)

	cfg := &config.Config{
		CodexKey: []internalconfig.CodexKey{{
			APIKey:  "opencode-go-key",
			BaseURL: "https://opencode.example/v1",
			Models: []internalconfig.CodexModel{
				{Name: model, Alias: model},
				{Name: "deepseek-v4.1-flash", Alias: "deepseek-v4.1-flash"},
			},
		}},
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "z.ai",
			BaseURL: "https://api.z.ai/api/coding/paas/v4",
			Models:  []config.OpenAICompatibilityModel{{Name: model, Alias: model}},
		}},
	}

	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{cfg: cfg, coreManager: manager}

	reg := internalregistry.GetGlobalRegistry()
	reg.UnregisterClient(codexAuthID)
	reg.UnregisterClient(zaiAuthID)
	t.Cleanup(func() {
		reg.UnregisterClient(codexAuthID)
		reg.UnregisterClient(zaiAuthID)
	})

	codexAuth := &coreauth.Auth{
		ID:       codexAuthID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			coreauth.AttributeAPIKey:      cfg.CodexKey[0].APIKey,
			coreauth.AttributeConfigIndex: "0",
			coreauth.AttributeSource:      "config:codex[0]",
			coreauth.AttributeAuthKind:    coreauth.AuthKindAPIKey,
			"base_url":                    cfg.CodexKey[0].BaseURL,
		},
	}
	zaiAuth := &coreauth.Auth{
		ID:       zaiAuthID,
		Provider: "openai-compatibility",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			coreauth.AttributeAPIKey:      "zai-key",
			coreauth.AttributeConfigIndex: "0",
			coreauth.AttributeSource:      "config:z.ai[0]",
			coreauth.AttributeAuthKind:    coreauth.AuthKindAPIKey,
			"base_url":                    cfg.OpenAICompatibility[0].BaseURL,
			"compat_name":                 "z.ai",
			"provider_key":                "z.ai",
		},
	}
	service.registerModelsForAuth(context.Background(), codexAuth)
	service.registerModelsForAuth(context.Background(), zaiAuth)
	if _, errRegister := manager.Register(context.Background(), codexAuth); errRegister != nil {
		t.Fatalf("register codex auth: %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), zaiAuth); errRegister != nil {
		t.Fatalf("register z.ai auth: %v", errRegister)
	}

	providers := util.GetProviderName(model)
	if len(providers) != 2 {
		t.Fatalf("providers before exhaustion = %v, want both codex and openai-compatible-z.ai", providers)
	}

	// Body captured from the live Z.ai coding-plan endpoint once the plan is spent.
	zaiExhausted := &coreauth.Error{
		HTTPStatus: http.StatusTooManyRequests,
		Message:    `{"error":{"code":"1310","message":"Weekly/Monthly Limit Exhausted. Your limit will reset at 2026-09-17 11:29:10"}}`,
	}
	manager.MarkResult(context.Background(), coreauth.Result{
		AuthID:   zaiAuthID,
		Provider: "openai-compatible-z.ai",
		Model:    model,
		Success:  false,
		Error:    zaiExhausted,
	})

	// The catalog must keep the model while the only other credential still serves it.
	if infos := reg.GetAvailableModelInfos(); !containsModelID(infos, model) {
		t.Fatalf("model %q disappeared from available models after Z.ai exhaustion", model)
	}
	if providers := util.GetProviderName(model); len(providers) != 2 {
		t.Fatalf("providers after exhaustion = %v, want both providers retained", providers)
	}
	registeredProviders := reg.GetModelProviders(model)
	if len(registeredProviders) != 2 {
		t.Fatalf("registered providers after exhaustion = %v, want both providers retained", registeredProviders)
	}
	if count := reg.GetModelCount(model); count != 0 {
		t.Fatalf("model count after exhaustion = %d, want 0 (cooling credential stays out of the available count)", count)
	}

	var attempts []string
	manager.RegisterExecutor(&serviceMixedProviderExecutor{
		identifier: "codex",
		execute: func(auth *coreauth.Auth, _ cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
			attempts = append(attempts, auth.ID)
			return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
		},
	})
	manager.RegisterExecutor(&serviceMixedProviderExecutor{
		identifier: "openai-compatible-z.ai",
		execute: func(auth *coreauth.Auth, _ cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
			attempts = append(attempts, auth.ID)
			return cliproxyexecutor.Response{}, zaiExhausted
		},
	})

	resp, errExecute := manager.Execute(context.Background(), providers, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if string(resp.Payload) != codexAuthID {
		t.Fatalf("Execute() payload = %q, want Codex credential %q (attempts=%v)", resp.Payload, codexAuthID, attempts)
	}
}

func containsModelID(models []*internalregistry.ModelInfo, modelID string) bool {
	for _, model := range models {
		if model != nil && model.ID == modelID {
			return true
		}
	}
	return false
}
