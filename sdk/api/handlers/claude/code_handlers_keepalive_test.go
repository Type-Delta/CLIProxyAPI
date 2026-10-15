package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// delayedClaudeStreamExecutor holds its first outcome for delay, then streams one
// Claude event or fails with err.
type delayedClaudeStreamExecutor struct {
	delay time.Duration
	err   *coreauth.Error
}

func (e *delayedClaudeStreamExecutor) Identifier() string { return "claude" }

func (e *delayedClaudeStreamExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "Execute not implemented"}
}

func (e *delayedClaudeStreamExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, 1)
	go func() {
		defer close(chunks)
		select {
		case <-time.After(e.delay):
		case <-ctx.Done():
			return
		}
		if e.err != nil {
			chunks <- coreexecutor.StreamChunk{Err: e.err}
			return
		}
		chunks <- coreexecutor.StreamChunk{Payload: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")}
	}()
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *delayedClaudeStreamExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *delayedClaudeStreamExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *delayedClaudeStreamExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

func runClaudeKeepAliveRequest(t *testing.T, executor *delayedClaudeStreamExecutor, keepAliveSeconds int) *httptest.ResponseRecorder {
	t.Helper()
	const model = "claude-keepalive-test-model"
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "claude-keepalive-test-auth", Provider: "claude", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("manager.Register: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		Streaming: sdkconfig.StreamingConfig{KeepAliveSeconds: keepAliveSeconds},
	}, manager)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"`+model+`","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	NewClaudeCodeAPIHandler(base).ClaudeMessages(c)
	return recorder
}

func TestClaudeStreamKeepAliveBeforeFirstChunk(t *testing.T) {
	recorder := runClaudeKeepAliveRequest(t, &delayedClaudeStreamExecutor{delay: 2500 * time.Millisecond}, 1)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := strings.Count(body, ": keep-alive\n\n"); got < 2 {
		t.Fatalf("keep-alive comments before first chunk = %d, want >= 2; body=%q", got, body)
	}
	if !strings.HasPrefix(body, ": keep-alive\n\n") || !strings.HasSuffix(body, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") {
		t.Fatalf("unexpected body ordering: %q", body)
	}
}

func TestClaudeStreamLateErrorAfterKeepAliveIsInStream(t *testing.T) {
	executor := &delayedClaudeStreamExecutor{delay: 1500 * time.Millisecond, err: &coreauth.Error{
		Code: "unavailable", Message: "all credentials cooling down", HTTPStatus: http.StatusServiceUnavailable,
	}}
	recorder := runClaudeKeepAliveRequest(t, executor, 1)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 because keep-alives committed the stream", recorder.Code)
	}
	if !strings.HasPrefix(body, ": keep-alive\n\n") || strings.Count(body, "event: error\ndata: ") != 1 {
		t.Fatalf("want keep-alive then one in-stream error event; body=%q", body)
	}
	if !strings.Contains(body, "all credentials cooling down") {
		t.Fatalf("in-stream error lost the upstream message; body=%q", body)
	}
}

func TestClaudeStreamFastErrorKeepsStatus(t *testing.T) {
	executor := &delayedClaudeStreamExecutor{err: &coreauth.Error{
		Code: "unavailable", Message: "all credentials cooling down", HTTPStatus: http.StatusServiceUnavailable,
	}}
	recorder := runClaudeKeepAliveRequest(t, executor, 60)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for an error that beat the keep-alive interval", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "keep-alive") || strings.Contains(recorder.Body.String(), "event: error") {
		t.Fatalf("fast error should be a plain JSON error body; body=%q", recorder.Body.String())
	}
}
