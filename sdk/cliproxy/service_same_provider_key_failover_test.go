package cliproxy

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// opencodeGoLimitError mirrors the upstream 429 body CPA receives from OpenCode
// Go when a workspace's weekly plan limit is spent, including the multi-day
// Retry-After signal CPA turns into a quota cooldown.
type opencodeGoLimitError struct {
	retryAfter time.Duration
}

func (e opencodeGoLimitError) Error() string {
	return `{"type":"error","error":{"type":"GoUsageLimitError","message":"Weekly usage limit reached."}}`
}

func (e opencodeGoLimitError) StatusCode() int { return http.StatusTooManyRequests }

func (e opencodeGoLimitError) RetryAfter() *time.Duration {
	retryAfter := e.retryAfter
	return &retryAfter
}

// TestServiceSameProviderKeysFailOver reproduces the production OpenCode Go
// layout: two API keys under one OpenAI-compatible provider advertising the same
// model. When the first key's weekly limit is exhausted, the second key must
// serve the request instead of failing the whole provider.
func TestServiceSameProviderKeysFailOver(t *testing.T) {
	const (
		model       = "deepseek-v4.1-flash"
		compatName  = "opencode go"
		providerKey = "openai-compatible-opencode go"
		keyAID      = "opencode-go-key-a"
		keyBID      = "opencode-go-key-b"
	)

	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:          compatName,
			BaseURL:       "https://opencode.example/go/v1",
			APIKeyEntries: []internalconfig.OpenAICompatibilityAPIKey{{APIKey: "go-key-a"}, {APIKey: "go-key-b"}},
			Models:        []config.OpenAICompatibilityModel{{Name: model, Alias: model}},
		}},
	}

	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.SetRetryConfig(3, 0, 0)
	service := &Service{cfg: cfg, coreManager: manager}

	makeKeyAuth := func(id, key string) *coreauth.Auth {
		return &coreauth.Auth{
			ID:       id,
			Provider: providerKey,
			Status:   coreauth.StatusActive,
			Attributes: map[string]string{
				coreauth.AttributeAPIKey: key,
				"base_url":               cfg.OpenAICompatibility[0].BaseURL,
				"compat_name":            compatName,
				"provider_key":           providerKey,
				"config_index":           "0",
			},
		}
	}

	reg := internalregistry.GetGlobalRegistry()
	reg.UnregisterClient(keyAID)
	reg.UnregisterClient(keyBID)
	t.Cleanup(func() {
		reg.UnregisterClient(keyAID)
		reg.UnregisterClient(keyBID)
	})
	for _, auth := range []*coreauth.Auth{makeKeyAuth(keyAID, "go-key-a"), makeKeyAuth(keyBID, "go-key-b")} {
		service.registerModelsForAuth(context.Background(), auth)
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.ID, errRegister)
		}
	}

	providers := util.GetProviderName(model)
	if len(providers) != 1 || providers[0] != providerKey {
		t.Fatalf("providers for model = %v, want [%s]", providers, providerKey)
	}

	attempts := make([]string, 0, 2)
	manager.RegisterExecutor(&serviceMixedProviderExecutor{
		identifier: providerKey,
		execute: func(auth *coreauth.Auth, _ cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
			attempts = append(attempts, auth.ID)
			if auth.ID == keyAID {
				return cliproxyexecutor.Response{}, opencodeGoLimitError{retryAfter: 91*time.Hour + 48*time.Minute}
			}
			return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
		},
	})

	resp, errExecute := manager.Execute(context.Background(), providers, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v, want failover to %s (attempts=%v)", errExecute, keyBID, attempts)
	}
	if string(resp.Payload) != keyBID {
		t.Fatalf("Execute() payload = %q, want healthy key %q (attempts=%v)", resp.Payload, keyBID, attempts)
	}
	if len(attempts) != 2 || attempts[0] != keyAID || attempts[1] != keyBID {
		t.Fatalf("attempts = %v, want [%s %s]", attempts, keyAID, keyBID)
	}

	// A follow-up request must keep using the healthy key instead of surfacing
	// the cooled-down key's quota as a 503.
	attempts = attempts[:0]
	resp, errExecute = manager.Execute(context.Background(), providers, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("second Execute() error = %v, want %s (attempts=%v)", errExecute, keyBID, attempts)
	}
	if string(resp.Payload) != keyBID {
		t.Fatalf("second Execute() payload = %q, want %q (attempts=%v)", resp.Payload, keyBID, attempts)
	}
	if fmt.Sprint(attempts) != fmt.Sprint([]string{keyBID}) {
		t.Fatalf("second attempts = %v, want only %s", attempts, keyBID)
	}
}
