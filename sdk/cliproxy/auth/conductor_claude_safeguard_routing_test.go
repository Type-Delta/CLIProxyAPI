package auth

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type claudeSafeguardRoutingExecutor struct {
	provider  string
	calls     *[]string
	claudeErr error
	observed  *error
}

func (e *claudeSafeguardRoutingExecutor) Identifier() string { return e.provider }
func (e *claudeSafeguardRoutingExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	*e.calls = append(*e.calls, e.provider)
	if e.provider == "claude" {
		*e.observed = e.claudeErr
		return cliproxyexecutor.Response{}, e.claudeErr
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}
func (e *claudeSafeguardRoutingExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}
func (e *claudeSafeguardRoutingExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	response, err := e.Execute(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: response.Payload}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}
func (*claudeSafeguardRoutingExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
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

type claudeSafeguardRejectedError struct{}

func (claudeSafeguardRejectedError) Error() string         { return "Claude OAuth safeguard rejected request" }
func (claudeSafeguardRejectedError) StatusCode() int       { return http.StatusForbidden }
func (claudeSafeguardRejectedError) IsRequestScoped() bool { return true }

func TestClaudeSafeguardMixedProviderRouting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		claudeErr error
		fallback  bool
	}{
		{"capture unavailable", claudeSafeguardCaptureUnavailableError{}, true},
		{"request rejected", claudeSafeguardRejectedError{}, false},
	} {
		for _, mode := range []string{"execute", "count", "stream"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				const model = "claude-opus-4-6"
				manager := NewManager(nil, nil, nil)
				calls := []string{}
				var observed error
				for _, provider := range []string{"claude", "kiro"} {
					id := "safeguard-routing-" + provider
					priority := "0"
					if provider == "claude" {
						priority = "10"
					}
					registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					_, err := manager.Register(context.Background(), &Auth{ID: id, Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": priority}})
					if err != nil {
						t.Fatal(err)
					}
					manager.RegisterExecutor(&claudeSafeguardRoutingExecutor{provider: provider, calls: &calls, claudeErr: tc.claudeErr, observed: &observed})
				}
				req := cliproxyexecutor.Request{Model: model}
				opts := cliproxyexecutor.Options{}
				var payload []byte
				var err error
				switch mode {
				case "execute":
					var response cliproxyexecutor.Response
					response, err = manager.Execute(context.Background(), []string{"claude", "kiro"}, req, opts)
					payload = response.Payload
				case "count":
					var response cliproxyexecutor.Response
					response, err = manager.ExecuteCount(context.Background(), []string{"claude", "kiro"}, req, opts)
					payload = response.Payload
				case "stream":
					var result *cliproxyexecutor.StreamResult
					result, err = manager.ExecuteStream(context.Background(), []string{"claude", "kiro"}, req, opts)
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
							payload = append(payload, chunk.Payload...)
						}
					}
				}
				if observed != tc.claudeErr {
					t.Fatalf("Claude error = %v, want %v", observed, tc.claudeErr)
				}
				if tc.fallback {
					if err != nil {
						t.Fatal(err)
					}
					if string(payload) != "safeguard-routing-kiro" {
						t.Fatalf("result = %q, want Kiro", payload)
					}
					if !reflect.DeepEqual(calls, []string{"claude", "kiro"}) {
						t.Fatalf("attempts = %v, want Claude then Kiro", calls)
					}
				} else {
					if err == nil || err.Error() != tc.claudeErr.Error() {
						t.Fatalf("error = %v, want %v", err, tc.claudeErr)
					}
					if !reflect.DeepEqual(calls, []string{"claude"}) {
						t.Fatalf("attempts = %v, want Claude only", calls)
					}
				}
			})
		}
	}
}
