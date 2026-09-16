package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestManager_ModelListPreservesCredentialScoped429(t *testing.T) {
	previousCooldown := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previousCooldown) })

	const (
		authID   = "model-list-credential-429-auth"
		provider = "openai"
		modelA   = "model-list-credential-429-a"
		modelB   = "model-list-credential-429-b"
	)

	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authID,
		Provider: provider,
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			modelA: {},
			modelB: {},
		},
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: modelA}, {ID: modelB}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	retryAfter := 30 * time.Second
	manager.MarkResult(context.Background(), Result{
		AuthID:          authID,
		Provider:        provider,
		Model:           modelA,
		Success:         false,
		CredentialScope: true,
		Error: &Error{
			Code:       "rate_limit_exceeded",
			Message:    "429 rate limit",
			HTTPStatus: http.StatusTooManyRequests,
		},
		RetryAfter: &retryAfter,
	})

	for _, model := range []string{modelA, modelB} {
		if !handlerModelAvailable(reg.GetAvailableModels("openai"), model) {
			t.Errorf("GetAvailableModels() did not list %q after credential-scoped 429", model)
		}
		if !providerModelAvailable(reg.GetAvailableModelsByProvider(provider), model) {
			t.Errorf("GetAvailableModelsByProvider() did not list %q after credential-scoped 429", model)
		}
	}

	snapshot, ok := manager.GetByID(authID)
	if !ok || snapshot == nil {
		t.Fatal("auth not found after MarkResult")
	}
	for _, model := range []string{modelA, modelB} {
		if blocked, _, _ := isAuthBlockedForModel(snapshot, model, time.Now()); !blocked {
			t.Errorf("model %q should remain blocked for routing during credential-scoped 429 cooldown", model)
		}
	}
}

func TestManager_ModelListPreservesTransient5xx(t *testing.T) {
	previousCooldown := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previousCooldown) })

	tests := []struct {
		name   string
		status int
	}{
		{name: "500", status: http.StatusInternalServerError},
		{name: "503", status: http.StatusServiceUnavailable},
		{name: "599", status: 599},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authID := "model-list-5xx-auth-" + test.name
			model := "model-list-5xx-" + test.name
			provider := "openai"

			manager := NewManager(nil, nil, nil)
			if _, errRegister := manager.Register(context.Background(), &Auth{
				ID:       authID,
				Provider: provider,
				Status:   StatusActive,
			}); errRegister != nil {
				t.Fatalf("register auth: %v", errRegister)
			}

			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(authID) })

			manager.MarkResult(context.Background(), Result{
				AuthID:   authID,
				Provider: provider,
				Model:    model,
				Success:  false,
				Error: &Error{
					Code:       "upstream_error",
					Message:    test.name + " upstream error",
					HTTPStatus: test.status,
				},
			})

			if !handlerModelAvailable(reg.GetAvailableModels("openai"), model) {
				t.Errorf("GetAvailableModels() did not list %q after HTTP %d", model, test.status)
			}
			if !providerModelAvailable(reg.GetAvailableModelsByProvider(provider), model) {
				t.Errorf("GetAvailableModelsByProvider() did not list %q after HTTP %d", model, test.status)
			}

			snapshot, ok := manager.GetByID(authID)
			if !ok || snapshot == nil {
				t.Fatal("auth not found after MarkResult")
			}
			if blocked, _, _ := isAuthBlockedForModel(snapshot, model, time.Now()); !blocked {
				t.Errorf("model %q should remain blocked for routing during HTTP %d cooldown", model, test.status)
			}
		})
	}
}

func TestManager_ModelListHidesDisabledCredential(t *testing.T) {
	const (
		authID   = "model-list-disabled-auth"
		provider = "openai"
		model    = "model-list-disabled"
	)

	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authID,
		Provider: provider,
		Status:   StatusDisabled,
		Disabled: true,
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	manager.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: provider,
		Model:    model,
		Success:  false,
		Error: &Error{
			Code:       "upstream_error",
			Message:    "500 upstream error",
			HTTPStatus: http.StatusInternalServerError,
		},
	})

	if handlerModelAvailable(reg.GetAvailableModels("openai"), model) {
		t.Errorf("GetAvailableModels() listed %q for a disabled credential", model)
	}
	if providerModelAvailable(reg.GetAvailableModelsByProvider(provider), model) {
		t.Errorf("GetAvailableModelsByProvider() listed %q for a disabled credential", model)
	}
}

func handlerModelAvailable(models []map[string]any, modelID string) bool {
	for _, model := range models {
		if id, ok := model["id"].(string); ok && id == modelID {
			return true
		}
	}
	return false
}

func providerModelAvailable(models []*registry.ModelInfo, modelID string) bool {
	for _, model := range models {
		if model != nil && model.ID == modelID {
			return true
		}
	}
	return false
}
