package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type mixedProviderProbeExecutor struct {
	identifier string
	execute    func(auth *Auth, req cliproxyexecutor.Request) (cliproxyexecutor.Response, error)
	stream     func(auth *Auth, req cliproxyexecutor.Request) (*cliproxyexecutor.StreamResult, error)
}

func (e *mixedProviderProbeExecutor) Identifier() string { return e.identifier }

func (e *mixedProviderProbeExecutor) Execute(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.execute == nil {
		return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
	}
	return e.execute(auth, req)
}

func (e *mixedProviderProbeExecutor) ExecuteStream(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if e.stream == nil {
		chunks := make(chan cliproxyexecutor.StreamChunk, 1)
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
		close(chunks)
		return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
	}
	return e.stream(auth, req)
}

func (e *mixedProviderProbeExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *mixedProviderProbeExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (e *mixedProviderProbeExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

// registerExhaustedZAIAndHealthyCodex registers the production layout: one OpenAI-compatible
// credential and one Codex credential that both advertise the same model, with the
// OpenAI-compatible credential already cooling down on a credential-scoped quota error.
func registerExhaustedZAIAndHealthyCodex(t *testing.T) (*Manager, string, string, string) {
	t.Helper()
	withQuotaCooldownEnabled(t)

	const (
		authIDZAI    = "mixed-failover-zai"
		authIDCodex  = "mixed-failover-codex"
		model        = "mixed-failover-glm-5.3-flash"
		providerZAI  = "openai-compatible-z.ai"
		providerCode = "codex"
	)

	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(3, 0, 0)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authIDZAI, providerZAI, []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(authIDCodex, providerCode, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(authIDZAI)
		reg.UnregisterClient(authIDCodex)
	})

	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authIDZAI,
		Provider: "openai-compatibility",
		Status:   StatusActive,
		Attributes: map[string]string{
			"compat_name":  "z.ai",
			"provider_key": providerZAI,
		},
	}); errRegister != nil {
		t.Fatalf("register z.ai auth: %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authIDCodex,
		Provider: providerCode,
		Status:   StatusActive,
	}); errRegister != nil {
		t.Fatalf("register Codex auth: %v", errRegister)
	}

	recoverAfter := time.Hour
	manager.MarkResult(context.Background(), Result{
		AuthID:          authIDZAI,
		Provider:        providerZAI,
		Model:           model,
		Success:         false,
		CredentialScope: true,
		Error:           &Error{HTTPStatus: http.StatusTooManyRequests, Message: "insufficient credits"},
		RetryAfter:      &recoverAfter,
	})

	return manager, authIDZAI, authIDCodex, model
}

func TestExecuteMixedProvidersFallsBackToReadyProvider(t *testing.T) {
	manager, authIDZAI, authIDCodex, model := registerExhaustedZAIAndHealthyCodex(t)

	var attempts []string
	manager.RegisterExecutor(&mixedProviderProbeExecutor{
		identifier: "codex",
		execute: func(auth *Auth, _ cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
			attempts = append(attempts, auth.ID)
			if auth.ID == authIDZAI {
				t.Fatalf("exhausted Z.ai credential was selected while Codex was ready")
			}
			return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
		},
	})
	manager.RegisterExecutor(&mixedProviderProbeExecutor{
		identifier: "openai-compatible-z.ai",
		execute: func(auth *Auth, _ cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
			attempts = append(attempts, auth.ID)
			return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
		},
	})

	resp, errExecute := manager.Execute(context.Background(), []string{"openai-compatible-z.ai", "codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if string(resp.Payload) != authIDCodex {
		t.Fatalf("Execute() payload = %q, want Codex credential %q", resp.Payload, authIDCodex)
	}
	if len(attempts) != 1 || attempts[0] != authIDCodex {
		t.Fatalf("attempts = %v, want only the ready Codex credential", attempts)
	}
}

func TestExecuteStreamMixedProvidersFallsBackToReadyProvider(t *testing.T) {
	manager, authIDZAI, authIDCodex, model := registerExhaustedZAIAndHealthyCodex(t)

	var attempts []string
	manager.RegisterExecutor(&mixedProviderProbeExecutor{
		identifier: "codex",
		stream: func(auth *Auth, _ cliproxyexecutor.Request) (*cliproxyexecutor.StreamResult, error) {
			attempts = append(attempts, auth.ID)
			if auth.ID == authIDZAI {
				t.Fatalf("exhausted Z.ai credential was selected while Codex was ready")
			}
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
		},
	})
	manager.RegisterExecutor(&mixedProviderProbeExecutor{
		identifier: "openai-compatible-z.ai",
		stream: func(auth *Auth, _ cliproxyexecutor.Request) (*cliproxyexecutor.StreamResult, error) {
			attempts = append(attempts, auth.ID)
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
		},
	})

	result, errStream := manager.ExecuteStream(context.Background(), []string{"openai-compatible-z.ai", "codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	var payloads []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		payloads = append(payloads, string(chunk.Payload))
	}
	if len(payloads) != 1 || payloads[0] != authIDCodex {
		t.Fatalf("stream payloads = %v, want only %q", payloads, authIDCodex)
	}
	if len(attempts) != 1 || attempts[0] != authIDCodex {
		t.Fatalf("attempts = %v, want only the ready Codex credential", attempts)
	}
}

// TestModelProvidersDisappearWhenOnlyExhaustedCredentialAdvertisesModel pins the
// failure mode behind the production symptom: if the Codex credential never
// registers the model, exhausting the only advertising credential removes the
// provider list and therefore the catalog entry.
func TestModelProvidersDisappearWhenOnlyExhaustedCredentialAdvertisesModel(t *testing.T) {
	withQuotaCooldownEnabled(t)

	const (
		authIDZAI   = "mixed-gap-zai"
		authIDCodex = "mixed-gap-codex"
		model       = "mixed-gap-glm-5.3-flash"
		providerZAI = "openai-compatible-z.ai"
	)

	manager := NewManager(nil, nil, nil)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authIDZAI, providerZAI, []*registry.ModelInfo{{ID: model}})
	// The Codex credential deliberately registers no models, matching a credential
	// whose configured model list does not declare glm-5.3-flash.
	reg.RegisterClient(authIDCodex, "codex", []*registry.ModelInfo{{ID: "gpt-5.5"}})
	t.Cleanup(func() {
		reg.UnregisterClient(authIDZAI)
		reg.UnregisterClient(authIDCodex)
	})

	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authIDZAI,
		Provider: "openai-compatibility",
		Status:   StatusActive,
		Attributes: map[string]string{
			"compat_name":  "z.ai",
			"provider_key": providerZAI,
		},
	}); errRegister != nil {
		t.Fatalf("register z.ai auth: %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authIDCodex,
		Provider: "codex",
		Status:   StatusActive,
	}); errRegister != nil {
		t.Fatalf("register Codex auth: %v", errRegister)
	}

	if providers := reg.GetModelProviders(model); len(providers) != 1 || providers[0] != providerZAI {
		t.Fatalf("providers before exhaustion = %v, want [%s]", providers, providerZAI)
	}

	recoverAfter := time.Hour
	manager.MarkResult(context.Background(), Result{
		AuthID:          authIDZAI,
		Provider:        providerZAI,
		Model:           model,
		Success:         false,
		CredentialScope: true,
		Error:           &Error{HTTPStatus: http.StatusTooManyRequests, Message: "insufficient credits"},
		RetryAfter:      &recoverAfter,
	})

	// The coalesced catalog view is what removes the model; the per-credential
	// registration above is untouched, but no other credential advertises it.
	if count := reg.GetModelCount(model); count != 0 {
		t.Fatalf("model count after exhaustion = %d, want 0 (only credential is cooling down)", count)
	}
	if _, _, _, errPick := manager.pickNextMixed(context.Background(), []string{providerZAI, "codex"}, model, cliproxyexecutor.Options{}, nil); errPick == nil {
		t.Fatal("expected mixed routing to fail because Codex never registered the model")
	}
}
