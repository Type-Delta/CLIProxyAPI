package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type claudeSafeguardRoutingExecutor struct {
	provider string
}

func (e *claudeSafeguardRoutingExecutor) Identifier() string { return e.provider }

func (e *claudeSafeguardRoutingExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.provider == "claude" {
		return cliproxyexecutor.Response{}, claudeSafeguardCaptureUnavailableError{}
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (*claudeSafeguardRoutingExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (*claudeSafeguardRoutingExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (*claudeSafeguardRoutingExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (*claudeSafeguardRoutingExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

type claudeSafeguardCaptureUnavailableError struct{}

func (claudeSafeguardCaptureUnavailableError) Error() string {
	return "Claude OAuth safeguard requires a current Claude Code capture"
}
func (claudeSafeguardCaptureUnavailableError) StatusCode() int          { return http.StatusServiceUnavailable }
func (claudeSafeguardCaptureUnavailableError) IsCredentialScoped() bool { return true }

func TestExecuteMixedProvidersFallsBackToKiroWhenClaudeSafeguardCannotInitialize(t *testing.T) {
	const model = "claude-opus-4-6"
	manager := NewManager(nil, nil, nil)
	claudeID, kiroID := "claude-safeguard-unavailable", "kiro-safeguard-fallback"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(claudeID, "claude", []*registry.ModelInfo{{ID: model}})
	reg.RegisterClient(kiroID, "kiro", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(claudeID)
		reg.UnregisterClient(kiroID)
	})
	for _, auth := range []*Auth{
		{ID: claudeID, Provider: "claude", Status: StatusActive},
		{ID: kiroID, Provider: "kiro", Status: StatusActive},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.Provider, errRegister)
		}
	}
	manager.RegisterExecutor(&claudeSafeguardRoutingExecutor{provider: "claude"})
	manager.RegisterExecutor(&claudeSafeguardRoutingExecutor{provider: "kiro"})
	manager.MarkResult(context.Background(), Result{
		AuthID: claudeID, Provider: "claude", Model: model, Success: false,
		Error: &Error{HTTPStatus: http.StatusUnauthorized, Message: "expired Claude OAuth token"},
	})

	resp, errExecute := manager.Execute(context.Background(), []string{"claude", "kiro"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := string(resp.Payload); got != kiroID {
		t.Fatalf("Execute() selected %q, want Kiro credential %q", got, kiroID)
	}
}
