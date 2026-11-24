package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// attemptUsageCapture collects records synchronously, so a test can require exactly one
// record per attempt.
type attemptUsageCapture struct {
	alias   string
	mu      sync.Mutex
	records []coreusage.Record
}

func newAttemptUsageCapture(t *testing.T, alias string) *attemptUsageCapture {
	t.Helper()
	capture := &attemptUsageCapture{alias: alias}
	unregister, err := coreusage.RegisterAccountingNamedPlugin(t.Name(), capture)
	if err != nil {
		t.Fatalf("register usage capture: %v", err)
	}
	t.Cleanup(func() { _ = unregister(context.Background()) })
	return capture
}

func (c *attemptUsageCapture) HandleUsage(_ context.Context, record coreusage.Record) {
	if record.Alias != c.alias {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, record)
}

// takeOne returns the only record published since the last call.
func (c *attemptUsageCapture) takeOne(t *testing.T, mode string) coreusage.Record {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.records) != 1 {
		t.Fatalf("%s published %d usage records, want 1", mode, len(c.records))
	}
	record := c.records[0]
	c.records = nil
	return record
}

type attemptFailureExecutor interface {
	Execute(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	ExecuteStream(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
}

// Local refusals happen before any upstream request. Each must still publish one failed
// attempt so analytics shows the credential the conductor tried before falling back.
func TestLocalCredentialRefusalsPublishFailedAttempts(t *testing.T) {
	responses := []byte(`{"model":"gpt-5","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	responsesOpts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), ResponseFormat: sdktranslator.FromString("openai-response")}
	compactOpts := responsesOpts
	compactOpts.Alt = "responses/compact"
	imageOpts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-image"), Metadata: map[string]any{"request_path": "/v1/images/generations"}}
	siwcAttributes := map[string]string{"use_siwc": "true"}
	// An apply_patch call whose input is an object fails xAI request preparation.
	xaiBadPatch := []byte(`{"model":"grok-4","input":[{"type":"custom_tool_call","name":"apply_patch","call_id":"call-1","input":{"patch":1}}]}`)
	// Requests that pass preparation by mistake reach this server instead of a real upstream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("request reached upstream, want a local refusal")
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(upstream.Close)
	xaiAuth := &cliproxyauth.Auth{ID: "xai-prepare", Provider: "xai", Attributes: map[string]string{"api_key": "key", "base_url": upstream.URL}}
	tests := []struct {
		name      string
		executor  attemptFailureExecutor
		auth      *cliproxyauth.Auth
		req       cliproxyexecutor.Request
		opts      cliproxyexecutor.Options
		modes     []string
		setup     func(*testing.T)
		status    int // 0 when the refusal carries no HTTP status
		wantModel string
	}{
		{
			name:     "codex siwc token missing",
			executor: NewCodexExecutor(&config.Config{}),
			auth:     &cliproxyauth.Auth{ID: "codex-siwc", Provider: "codex", Attributes: siwcAttributes},
			req:      cliproxyexecutor.Request{Model: "gpt-5", Payload: responses},
			opts:     responsesOpts,
			status:   http.StatusUnauthorized,
		},
		{
			name:     "codex websocket siwc token missing",
			executor: NewCodexWebsocketsExecutor(&config.Config{}),
			auth:     &cliproxyauth.Auth{ID: "codex-ws-siwc", Provider: "codex", Attributes: siwcAttributes},
			req:      cliproxyexecutor.Request{Model: "gpt-5", Payload: responses},
			opts:     responsesOpts,
			modes:    []string{"execute"},
			status:   http.StatusUnauthorized,
		},
		{
			name:      "codex direct image siwc token missing",
			executor:  NewCodexExecutor(&config.Config{}),
			auth:      &cliproxyauth.Auth{ID: "codex-image-siwc", Provider: "codex", Attributes: siwcAttributes},
			req:       cliproxyexecutor.Request{Model: "gpt-image-2.6-sunburst", Payload: []byte(`{"model":"gpt-image-2.6-sunburst","prompt":"cat"}`)},
			opts:      imageOpts,
			status:    http.StatusUnauthorized,
			wantModel: "gpt-image-2.6-sunburst",
		},
		{
			name:     "xai request preparation failure",
			executor: NewXAIExecutor(&config.Config{}),
			auth:     xaiAuth,
			req:      cliproxyexecutor.Request{Model: "grok-4", Payload: xaiBadPatch},
			opts:     responsesOpts,
		},
		{
			name:     "xai compact request preparation failure",
			executor: NewXAIExecutor(&config.Config{}),
			auth:     xaiAuth,
			req:      cliproxyexecutor.Request{Model: "grok-4", Payload: xaiBadPatch},
			opts:     compactOpts,
			modes:    []string{"execute"},
		},
		{
			name:     "xai websocket request preparation failure",
			executor: NewXAIWebsocketsExecutor(&config.Config{}),
			auth:     xaiAuth,
			req:      cliproxyexecutor.Request{Model: "grok-4", Payload: xaiBadPatch},
			opts:     responsesOpts,
			modes:    []string{"stream"},
		},
		{
			name:     "meta credential missing token",
			executor: NewMetaExecutor(&config.Config{}),
			auth:     &cliproxyauth.Auth{ID: "meta-missing", Provider: "meta"},
			req:      cliproxyexecutor.Request{Model: "muse-spark", Payload: responses},
			opts:     responsesOpts,
			status:   http.StatusUnauthorized,
		},
		{
			name:     "antigravity kv store unavailable",
			executor: NewAntigravityExecutor(&config.Config{}),
			auth:     &cliproxyauth.Auth{ID: "antigravity-kv", Provider: "antigravity", Metadata: map[string]any{"access_token": "token", "project_id": "project"}},
			req:      cliproxyexecutor.Request{Model: "gemini-3-pro", Payload: []byte(`{"model":"gemini-3-pro","messages":[{"role":"user","content":"hi"}]}`)},
			opts:     cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI},
			setup: func(t *testing.T) {
				useFakeAntigravityKVClient(t, nil, true, errors.New("kv down"))
			},
			status: http.StatusServiceUnavailable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.setup != nil {
				test.setup(t)
			}
			alias := "attempt-failure-" + test.name
			capture := newAttemptUsageCapture(t, alias)
			ctx := coreusage.WithRequestedModelAlias(context.Background(), alias)
			runs := map[string]func() error{
				"execute": func() error { _, err := test.executor.Execute(ctx, test.auth, test.req, test.opts); return err },
				"stream": func() error {
					stream, err := test.executor.ExecuteStream(ctx, test.auth, test.req, test.opts)
					if stream != nil {
						for range stream.Chunks {
						}
					}
					return err
				},
			}
			modes := test.modes
			if modes == nil {
				modes = []string{"execute", "stream"}
			}
			for _, mode := range modes {
				errRun := runs[mode]()
				if errRun == nil {
					t.Fatalf("%s error = nil, want local refusal", mode)
				}
				record := capture.takeOne(t, mode)
				if !record.Failed || record.Fail.StatusCode != test.status || record.UpstreamStatusCode != 0 || record.FailureHop != "cpa_to_provider" {
					t.Fatalf("%s record failed=%t status=%d upstream=%d hop=%q, want local %d", mode, record.Failed, record.Fail.StatusCode, record.UpstreamStatusCode, record.FailureHop, test.status)
				}
				if record.Fail.Body != errRun.Error() {
					t.Fatalf("%s record body = %q, want %q", mode, record.Fail.Body, errRun.Error())
				}
				if test.wantModel != "" && record.Model != test.wantModel {
					t.Fatalf("%s record model = %q, want %q", mode, record.Model, test.wantModel)
				}
			}
		})
	}
}

func TestPrepareFailureReportersPublishFailedAttempts(t *testing.T) {
	reporters := map[string]cliproxyauth.PrepareFailureReporter{
		"claude":      NewClaudeExecutor(&config.Config{}),
		"antigravity": NewAntigravityExecutor(&config.Config{}),
		"meta":        NewMetaExecutor(&config.Config{}),
	}
	for name, reporter := range reporters {
		t.Run(name, func(t *testing.T) {
			alias := "prepare-failure-" + name
			capture := newAttemptUsageCapture(t, alias)
			ctx := coreusage.WithRequestedModelAlias(context.Background(), alias)
			errPrepare := errors.New("token refresh failed")
			reporter.ReportPrepareFailure(ctx, &cliproxyauth.Auth{ID: name + "-auth", Provider: name}, "model-a(high)", errPrepare)
			record := capture.takeOne(t, "report")
			if !record.Failed || record.Fail.Body != errPrepare.Error() || record.Model != "model-a" || record.FailureHop != "cpa_to_provider" {
				t.Fatalf("record failed=%t body=%q model=%q hop=%q", record.Failed, record.Fail.Body, record.Model, record.FailureHop)
			}
		})
	}
}
