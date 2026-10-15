package handlers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// slowFirstChunkExecutor controls how long stream setup and the first chunk take.
// Releasing setup returns the stream result; releasing first sends the first outcome.
type slowFirstChunkExecutor struct {
	setup     chan struct{}
	first     chan struct{}
	failFirst *coreauth.Error
	failSetup *coreauth.Error
}

func (e *slowFirstChunkExecutor) Identifier() string { return "codex" }

func (e *slowFirstChunkExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "Execute not implemented"}
}

func (e *slowFirstChunkExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	select {
	case <-e.setup:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if e.failSetup != nil {
		return nil, e.failSetup
	}
	chunks := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(chunks)
		select {
		case <-e.first:
		case <-ctx.Done():
			return
		}
		if e.failFirst != nil {
			chunks <- coreexecutor.StreamChunk{Err: e.failFirst}
			return
		}
		chunks <- coreexecutor.StreamChunk{Payload: []byte("first")}
		chunks <- coreexecutor.StreamChunk{Payload: []byte("second")}
	}()
	return &coreexecutor.StreamResult{Headers: http.Header{"X-Upstream": {"1"}}, Chunks: chunks}, nil
}

func (e *slowFirstChunkExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *slowFirstChunkExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *slowFirstChunkExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

// flushRecorder records every flushed write so tests can observe bytes sent before the
// helper returns.
type flushRecorder struct {
	*httptest.ResponseRecorder
	mu      sync.Mutex
	flushes []string
	flushed chan struct{}
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}, 64)}
}

func (r *flushRecorder) Flush() {
	r.mu.Lock()
	r.flushes = append(r.flushes, r.Body.String())
	r.mu.Unlock()
	r.ResponseRecorder.Flush()
	r.flushed <- struct{}{}
}

func (r *flushRecorder) waitFlush(t *testing.T) {
	t.Helper()
	select {
	case <-r.flushed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a keep-alive flush")
	}
}

func newSlowFirstChunkHandler(t *testing.T, keepAliveSeconds int, executor *slowFirstChunkExecutor) *BaseAPIHandler {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "slow-first-chunk-auth", Provider: "codex", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("manager.Register: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "slow-first-chunk-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	return NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		Streaming: sdkconfig.StreamingConfig{KeepAliveSeconds: keepAliveSeconds},
	}, manager)
}

type firstChunkCall struct {
	recorder *flushRecorder
	result   chan FirstChunkStream
	cancel   context.CancelFunc
}

func startFirstChunkCall(t *testing.T, handler *BaseAPIHandler) *firstChunkCall {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := newFlushRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	setSSEHeaders := func() { c.Header("Content-Type", "text/event-stream") }
	result := make(chan FirstChunkStream, 1)
	go func() {
		result <- handler.ExecuteStreamWithFirstChunkKeepAlive(c, ctx, "openai", "slow-first-chunk-model", []byte(`{"model":"slow-first-chunk-model"}`), "", setSSEHeaders)
	}()
	return &firstChunkCall{recorder: recorder, result: result, cancel: cancel}
}

func (call *firstChunkCall) wait(t *testing.T) FirstChunkStream {
	t.Helper()
	select {
	case stream := <-call.result:
		return stream
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not return")
		return FirstChunkStream{}
	}
}

func drainChunks(t *testing.T, data <-chan []byte) string {
	t.Helper()
	var got []string
	for chunk := range data {
		got = append(got, string(chunk))
	}
	return strings.Join(got, ",")
}

func TestFirstChunkKeepAlive_WritesCommentsWhileSetupIsPending(t *testing.T) {
	executor := &slowFirstChunkExecutor{setup: make(chan struct{}), first: make(chan struct{})}
	call := startFirstChunkCall(t, newSlowFirstChunkHandler(t, 1, executor))

	// Credential selection, cooldown waits and retries all happen before setup returns.
	call.recorder.waitFlush(t)
	call.recorder.waitFlush(t)
	if got := call.recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type after keep-alive = %q, want text/event-stream", got)
	}
	if got := call.recorder.Code; got != http.StatusOK {
		t.Fatalf("status after keep-alive = %d, want 200", got)
	}
	if got := call.recorder.Body.String(); got != ": keep-alive\n\n: keep-alive\n\n" {
		t.Fatalf("body before first chunk = %q", got)
	}

	close(executor.setup)
	close(executor.first)
	stream := call.wait(t)
	if !stream.Committed {
		t.Fatal("Committed = false after keep-alives were written")
	}
	if got := drainChunks(t, stream.Data); got != "first,second" {
		t.Fatalf("chunks = %q, want first,second", got)
	}
}

func TestFirstChunkKeepAlive_WritesCommentsWhileFirstChunkIsPending(t *testing.T) {
	executor := &slowFirstChunkExecutor{setup: make(chan struct{}), first: make(chan struct{})}
	close(executor.setup)
	call := startFirstChunkCall(t, newSlowFirstChunkHandler(t, 1, executor))

	call.recorder.waitFlush(t)
	if got := call.recorder.Body.String(); !strings.HasPrefix(got, ": keep-alive\n\n") {
		t.Fatalf("body while upstream is silent = %q", got)
	}

	close(executor.first)
	stream := call.wait(t)
	if !stream.Committed {
		t.Fatal("Committed = false after keep-alives were written")
	}
	if got := drainChunks(t, stream.Data); got != "first,second" {
		t.Fatalf("chunks = %q, want first,second", got)
	}
	if strings.Contains(call.recorder.Body.String(), "first") {
		t.Fatal("helper wrote payload bytes; payload delivery belongs to the handler")
	}
}

func TestFirstChunkKeepAlive_FastOutcomeLeavesResponseUncommitted(t *testing.T) {
	for _, tt := range []struct {
		name     string
		executor *slowFirstChunkExecutor
	}{
		{name: "first chunk", executor: &slowFirstChunkExecutor{setup: make(chan struct{}), first: make(chan struct{})}},
		{name: "setup error", executor: &slowFirstChunkExecutor{setup: make(chan struct{}), first: make(chan struct{}),
			failSetup: &coreauth.Error{Code: "bad_request", Message: "bad request", HTTPStatus: http.StatusBadRequest}}},
		{name: "first chunk error", executor: &slowFirstChunkExecutor{setup: make(chan struct{}), first: make(chan struct{}),
			failFirst: &coreauth.Error{Code: "bad_request", Message: "bad request", HTTPStatus: http.StatusBadRequest}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			close(tt.executor.setup)
			close(tt.executor.first)
			call := startFirstChunkCall(t, newSlowFirstChunkHandler(t, 60, tt.executor))
			stream := call.wait(t)
			if stream.Committed {
				t.Fatal("Committed = true for an outcome that beat the keep-alive interval")
			}
			if call.recorder.Body.Len() != 0 || call.recorder.Header().Get("Content-Type") != "" {
				t.Fatalf("helper wrote before the interval: headers=%v body=%q", call.recorder.Header(), call.recorder.Body.String())
			}
			if tt.executor.failSetup == nil && tt.executor.failFirst == nil {
				if got := drainChunks(t, stream.Data); got != "first,second" {
					t.Fatalf("chunks = %q, want first,second", got)
				}
				return
			}
			var status int
			for errMsg := range stream.Errors {
				if errMsg != nil {
					status = errMsg.StatusCode
				}
			}
			if status != http.StatusBadRequest {
				t.Fatalf("error status = %d, want 400 so the handler can return it as-is", status)
			}
		})
	}
}

func TestFirstChunkKeepAlive_LateErrorIsRedeliveredAfterCommit(t *testing.T) {
	executor := &slowFirstChunkExecutor{setup: make(chan struct{}), first: make(chan struct{}),
		failFirst: &coreauth.Error{Code: "unavailable", Message: "all credentials cooling down", HTTPStatus: http.StatusServiceUnavailable}}
	close(executor.setup)
	call := startFirstChunkCall(t, newSlowFirstChunkHandler(t, 1, executor))

	call.recorder.waitFlush(t)
	close(executor.first)
	stream := call.wait(t)
	if !stream.Committed {
		t.Fatal("Committed = false after keep-alives were written")
	}
	var got []int
	for errMsg := range stream.Errors {
		if errMsg != nil {
			got = append(got, errMsg.StatusCode)
		}
	}
	if len(got) != 1 || got[0] != http.StatusServiceUnavailable {
		t.Fatalf("errors = %v, want one 503 for the in-stream error writer", got)
	}
}

func TestFirstChunkKeepAlive_DisabledIntervalKeepsSynchronousBehavior(t *testing.T) {
	executor := &slowFirstChunkExecutor{setup: make(chan struct{}), first: make(chan struct{})}
	call := startFirstChunkCall(t, newSlowFirstChunkHandler(t, 0, executor))
	select {
	case <-call.result:
		t.Fatal("helper returned before stream setup finished with keep-alives disabled")
	case <-time.After(100 * time.Millisecond):
	}
	close(executor.setup)
	close(executor.first)
	stream := call.wait(t)
	if stream.Committed || call.recorder.Body.Len() != 0 {
		t.Fatalf("disabled keep-alive wrote bytes: committed=%v body=%q", stream.Committed, call.recorder.Body.String())
	}
	if got := drainChunks(t, stream.Data); got != "first,second" {
		t.Fatalf("chunks = %q, want first,second", got)
	}
}

func TestFirstChunkKeepAlive_ClientDisconnectReturnsClosedChannels(t *testing.T) {
	executor := &slowFirstChunkExecutor{setup: make(chan struct{}), first: make(chan struct{})}
	handler := newSlowFirstChunkHandler(t, 1, executor)
	gin.SetMode(gin.TestMode)
	recorder := newFlushRecorder()
	c, _ := gin.CreateTestContext(recorder)
	reqCtx, disconnect := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", io.NopCloser(strings.NewReader(""))).WithContext(reqCtx)
	execCtx, cancelExec := context.WithCancel(context.Background())
	defer cancelExec()
	result := make(chan FirstChunkStream, 1)
	go func() {
		result <- handler.ExecuteStreamWithFirstChunkKeepAlive(c, execCtx, "openai", "slow-first-chunk-model", []byte(`{"model":"slow-first-chunk-model"}`), "", func() {})
	}()
	disconnect()
	select {
	case stream := <-result:
		if _, ok := <-stream.Data; ok {
			t.Fatal("Data is open after client disconnect")
		}
		if _, ok := <-stream.Errors; ok {
			t.Fatal("Errors is open after client disconnect")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not return after client disconnect")
	}
}
