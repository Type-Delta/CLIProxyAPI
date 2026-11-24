package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// antigravityNoFinishReasonSSE mirrors a real upstream stream captured from
// cloudcode-pa: a single tool-call chunk that carries usageMetadata but never
// emits finishReason.
const antigravityNoFinishReasonSSE = `data: {"response":{"candidates":[{"content":{"parts":[{"thoughtSignature":"CqcECqQEARFNMg8iZ3xVniKaulgBlzhJJDlc","functionCall":{"name":"bash","args":{"command":"ls -la .."},"id":"call_524881"}}],"role":"model"}}],"modelVersion":"gemini-3.7-flash","responseId":"resp-nofr","usageMetadata":{"promptTokenCount":153609,"candidatesTokenCount":17,"totalTokenCount":153720,"thoughtsTokenCount":94}}}
`

func newAntigravityNoFinishReasonServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, antigravityNoFinishReasonSSE)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
}

func collectAntigravityStream(t *testing.T, baseURL string, sourceFormat, responseFormat sdktranslator.Format, payload string) [][]byte {
	t.Helper()
	executor := NewAntigravityExecutor(&config.Config{
		Antigravity:  config.AntigravityConfig{},
		RequestRetry: 1,
	})
	result, errExecute := executor.ExecuteStream(context.Background(), &cliproxyauth.Auth{
		Metadata: map[string]any{
			"access_token": "token-123",
			"expired":      time.Now().Add(24 * time.Hour).Format(time.RFC3339),
			"project_id":   "project-1",
		},
		Attributes: map[string]string{"base_url": baseURL},
	}, cliproxyexecutor.Request{
		Model:   "gemini-3.7-flash",
		Payload: []byte(payload),
	}, cliproxyexecutor.Options{
		SourceFormat:   sourceFormat,
		ResponseFormat: responseFormat,
		Stream:         true,
	})
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	var chunks [][]byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		chunks = append(chunks, chunk.Payload)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
	return chunks
}

func TestAntigravityNativeStreamPreservesWireFrames(t *testing.T) {
	const partial = `{"response":{"candidates":[{"content":{"parts":[{"text":"OK"}]}}],"usageMetadata":{"promptTokenCount":2}}}`
	const terminal = `{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":3}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+partial+"\n\ndata: "+terminal+"\n\n")
	}))
	defer server.Close()

	const payload = `{"model":"gemini-3.7-flash","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`
	chunks := collectAntigravityStream(t, server.URL, sdktranslator.FormatAntigravity, sdktranslator.FormatAntigravity, payload)
	if got := string(bytes.Join(chunks, []byte("\n"))); got != partial+"\n"+terminal {
		t.Fatalf("native frames = %s", got)
	}
}

func TestAntigravityGroupedCooldownFollowsWireModel(t *testing.T) {
	const model = "gemini-3.8-flash"
	const high, low = model + "-high", model + "-low"
	const quota = `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"60s"}]}}`
	var highCalls, lowCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Error(errRead)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch ojson.GetBytes(body, "model").Str {
		case high:
			highCalls.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, quota)
		case low:
			lowCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"OK\"}]},\"finishReason\":\"STOP\"}]}}\n\n")
		default:
			t.Error("request did not resolve a wire model")
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	authID := t.Name()
	reg := registry.GetGlobalRegistry()
	group := &registry.AntigravityThinking{BaseID: model, DefaultLevel: "low", WireModels: map[string]string{"high": high, "low": low}}
	var models []*registry.ModelInfo
	for _, id := range []string{model, high, low} {
		models = append(models, &registry.ModelInfo{ID: id, Type: "antigravity", Thinking: &registry.ThinkingSupport{Levels: []string{"high", "low"}}, AntigravityThinking: group})
	}
	reg.RegisterClient(authID, "antigravity", models)
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
		antigravityShortCooldownByAuth.Delete(antigravityShortCooldownKey(&cliproxyauth.Auth{ID: authID}, high))
	})
	cfg := &config.Config{}
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(NewAntigravityExecutor(cfg))
	_, errRegister := manager.Register(t.Context(), &cliproxyauth.Auth{ID: authID, Provider: "antigravity",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "token", "project_id": "project", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)},
	})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, Stream: true}
	request := func(level string) cliproxyexecutor.Request {
		payload := strings.ReplaceAll(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"LEVEL"}}}}`, "LEVEL", level)
		return cliproxyexecutor.Request{Model: model, Payload: []byte(payload)}
	}
	if _, err := manager.ExecuteStream(t.Context(), []string{"antigravity"}, request("HIGH"), opts); err == nil {
		t.Fatal("expected high effort to return 429")
	}
	result, errLow := manager.ExecuteStream(t.Context(), []string{"antigravity"}, request("LOW"), opts)
	if errLow != nil {
		t.Fatalf("high effort cooldown blocked low effort: %v", errLow)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
	if _, err := manager.ExecuteStream(t.Context(), []string{"antigravity"}, request("HIGH"), opts); err == nil {
		t.Fatal("expected high effort cooldown")
	}
	if highCalls.Load() != 1 || lowCalls.Load() != 1 {
		t.Fatalf("upstream calls high=%d low=%d, want one each", highCalls.Load(), lowCalls.Load())
	}
}

func TestAntigravityStreamSurfacesTrailingBackendError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"parts":[{"text":"partial"}]}}],"modelVersion":"gemini-3.7-flash"}}

{
  "error": {
    "code": 503,
    "message": "backend capacity exhausted",
    "status": "UNAVAILABLE"
  }
}
`)
	}))
	defer server.Close()

	for _, format := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAI} {
		t.Run(format.String(), func(t *testing.T) {
			executor := NewAntigravityExecutor(&config.Config{
				Antigravity:  config.AntigravityConfig{},
				RequestRetry: 1,
			})
			result, errExecute := executor.ExecuteStream(context.Background(), &cliproxyauth.Auth{
				Metadata: map[string]any{
					"access_token": "token-123",
					"expired":      time.Now().Add(24 * time.Hour).Format(time.RFC3339),
					"project_id":   "project-1",
				},
				Attributes: map[string]string{"base_url": server.URL},
			}, cliproxyexecutor.Request{
				Model:   "gemini-3.7-flash",
				Payload: []byte(`{"model":"gemini-3.7-flash","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
			}, cliproxyexecutor.Options{
				SourceFormat:   format,
				ResponseFormat: format,
				Stream:         true,
			})
			if errExecute != nil {
				t.Fatalf("ExecuteStream() error = %v", errExecute)
			}

			var streamErr error
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					streamErr = chunk.Err
					continue
				}
				if bytes.Contains(chunk.Payload, []byte("message_stop")) || bytes.Contains(chunk.Payload, []byte(`"finish_reason":"stop"`)) {
					t.Fatalf("trailing backend error became successful completion: %s", chunk.Payload)
				}
			}
			if streamErr == nil || !bytes.Contains([]byte(streamErr.Error()), []byte("backend capacity exhausted")) {
				t.Fatalf("trailing backend error = %v, want surfaced backend message", streamErr)
			}
			statusError, ok := streamErr.(interface{ StatusCode() int })
			if !ok || statusError.StatusCode() != http.StatusServiceUnavailable {
				t.Fatalf("trailing backend status code = %v, want %d", streamErr, http.StatusServiceUnavailable)
			}
		})
	}
}

// TestAntigravityStreamDoesNotEmitClaudeMessageStopOnReadError locks in the
// executor ordering fix on the response format where it is observable: the
// Claude translator has always synthesized a terminal event on [DONE], so
// translating the tail before checking scanner.Err() reported a truncated
// upstream stream to Claude clients as a completed message.
func TestAntigravityStreamDoesNotEmitClaudeMessageStopOnReadError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}],"modelVersion":"gemini-3.7-flash","responseId":"resp-cut"}}`+"\n\n")
		flusher.Flush()
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, errHijack := hijacker.Hijack()
		if errHijack == nil {
			_ = conn.Close()
		}
	}))
	defer server.Close()

	executor := NewAntigravityExecutor(&config.Config{
		Antigravity:  config.AntigravityConfig{},
		RequestRetry: 1,
	})
	result, errExecute := executor.ExecuteStream(context.Background(), &cliproxyauth.Auth{
		Metadata: map[string]any{
			"access_token": "token-123",
			"expired":      time.Now().Add(24 * time.Hour).Format(time.RFC3339),
			"project_id":   "project-1",
		},
		Attributes: map[string]string{"base_url": server.URL},
	}, cliproxyexecutor.Request{
		Model:   "gemini-3.7-flash",
		Payload: []byte(`{"model":"gemini-3.7-flash","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"hello"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatClaude,
		ResponseFormat: sdktranslator.FormatClaude,
		Stream:         true,
	})
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			continue
		}
		if bytes.Contains(chunk.Payload, []byte("message_stop")) {
			t.Fatalf("read error must not emit message_stop: %s", chunk.Payload)
		}
	}
	if streamErr == nil {
		t.Fatal("expected stream read error")
	}
}

func TestAntigravityStreamDoesNotFinalizeEmptyUpstreamStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer server.Close()

	for _, tc := range []struct {
		name           string
		sourceFormat   sdktranslator.Format
		responseFormat sdktranslator.Format
		payload        string
	}{
		{"gemini", sdktranslator.FormatGemini, sdktranslator.FormatGemini,
			`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`},
		{"openai", sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAI,
			`{"model":"gemini-3.7-flash","messages":[{"role":"user","content":"hello"}],"stream":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := NewAntigravityExecutor(&config.Config{
				Antigravity:  config.AntigravityConfig{},
				RequestRetry: 1,
			})
			result, errExecute := executor.ExecuteStream(context.Background(), &cliproxyauth.Auth{
				Metadata: map[string]any{
					"access_token": "token-123",
					"expired":      time.Now().Add(24 * time.Hour).Format(time.RFC3339),
					"project_id":   "project-1",
				},
				Attributes: map[string]string{"base_url": server.URL},
			}, cliproxyexecutor.Request{
				Model:   "gemini-3.7-flash",
				Payload: []byte(tc.payload),
			}, cliproxyexecutor.Options{
				SourceFormat:   tc.sourceFormat,
				ResponseFormat: tc.responseFormat,
				Stream:         true,
			})
			if errExecute != nil {
				t.Fatalf("ExecuteStream() error = %v", errExecute)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("unexpected stream error: %v", chunk.Err)
				}
				if finish := ojson.GetBytes(chunk.Payload, "candidates.0.finishReason").String(); finish != "" {
					t.Fatalf("empty upstream stream must not synthesize a terminal chunk: %s", chunk.Payload)
				}
				if finish := ojson.GetBytes(chunk.Payload, "choices.0.finish_reason").String(); finish != "" {
					t.Fatalf("empty upstream stream must not synthesize a terminal chunk: %s", chunk.Payload)
				}
			}
		})
	}
}

func TestAntigravityStreamSynthesizesGeminiTerminalChunkWithUsage(t *testing.T) {
	server := newAntigravityNoFinishReasonServer(t)
	defer server.Close()

	chunks := collectAntigravityStream(t, server.URL, sdktranslator.FormatGemini, sdktranslator.FormatGemini,
		`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)

	terminal := chunks[len(chunks)-1]
	if fr := ojson.GetBytes(terminal, "candidates.0.finishReason").String(); fr != "STOP" {
		t.Fatalf("terminal finishReason = %q, want STOP", fr)
	}
	if role := ojson.GetBytes(terminal, "candidates.0.content.role").String(); role != "model" {
		t.Fatalf("terminal content role = %q, want model", role)
	}
	if got := ojson.GetBytes(terminal, "usageMetadata.totalTokenCount").Int(); got != 153720 {
		t.Fatalf("terminal totalTokenCount = %d, want 153720", got)
	}
	if got := ojson.GetBytes(terminal, "usageMetadata.promptTokenCount").Int(); got != 153609 {
		t.Fatalf("terminal promptTokenCount = %d, want 153609", got)
	}
	if got := ojson.GetBytes(terminal, "modelVersion").String(); got != "gemini-3.7-flash" {
		t.Fatalf("terminal modelVersion = %q, want gemini-3.7-flash", got)
	}
	if got := ojson.GetBytes(terminal, "responseId").String(); got != "resp-nofr" {
		t.Fatalf("terminal responseId = %q, want resp-nofr", got)
	}

	var finishReasonCount int
	for _, chunk := range chunks {
		if ojson.GetBytes(chunk, "candidates.0.finishReason").String() != "" {
			finishReasonCount++
		}
	}
	if finishReasonCount != 1 {
		t.Fatalf("expected exactly one finishReason chunk, got %d", finishReasonCount)
	}
}

func TestAntigravityStreamSynthesizesOpenAIToolCallsFinishReason(t *testing.T) {
	server := newAntigravityNoFinishReasonServer(t)
	defer server.Close()

	chunks := collectAntigravityStream(t, server.URL, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAI,
		`{"model":"gemini-3.7-flash","messages":[{"role":"user","content":"hello"}],"stream":true}`)

	terminal := chunks[len(chunks)-1]
	if fr := ojson.GetBytes(terminal, "choices.0.finish_reason").String(); fr != "tool_calls" {
		t.Fatalf("terminal finish_reason = %q, want tool_calls", fr)
	}
	if got := ojson.GetBytes(terminal, "usage.total_tokens").Int(); got != 153720 {
		t.Fatalf("terminal total_tokens = %d, want 153720", got)
	}
	if got := ojson.GetBytes(terminal, "usage.prompt_tokens").Int(); got != 153609 {
		t.Fatalf("terminal prompt_tokens = %d, want 153609", got)
	}
	if got := ojson.GetBytes(terminal, "model").String(); got != "gemini-3.7-flash" {
		t.Fatalf("terminal model = %q, want gemini-3.7-flash", got)
	}

	var finishReasonCount int
	for _, chunk := range chunks {
		if ojson.GetBytes(chunk, "choices.0.finish_reason").String() != "" {
			finishReasonCount++
		}
	}
	if finishReasonCount != 1 {
		t.Fatalf("expected exactly one finish_reason chunk, got %d", finishReasonCount)
	}
}

func TestAntigravityAssemblyReplacesOnlyContentParts(t *testing.T) {
	for _, metadata := range []string{
		`"groundingMetadata":{"webSearchQueries":[]}`,
		`"marker":"__cpa_parts_placeholder__"`,
	} {
		t.Run(metadata, func(t *testing.T) {
			raw := []byte(`{"response":{"candidates":[{` + metadata + `,"content":{"role":"model","parts":[]},"finishReason":"STOP"}]}}`)
			exec := &AntigravityExecutor{}
			first := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}}`)
			out := exec.convertStreamChunksToNonStream([][]byte{first, raw})
			if !ojson.ValidBytes(out) || ojson.GetBytes(out, "response.candidates.0.content.parts.0.text").String() != "hello" {
				t.Fatalf("assembled parts replaced the wrong value: %s", out)
			}
			before := ojson.GetBytes(raw, "response.candidates.0")
			after := ojson.GetBytes(out, "response.candidates.0")
			if before.Get("groundingMetadata").Raw != after.Get("groundingMetadata").Raw ||
				before.Get("marker").Raw != after.Get("marker").Raw {
				t.Fatalf("assembly changed unrelated metadata: %s", out)
			}
		})
	}
}

func TestAntigravityStreamPreservesQuotedFilterNoticeWithUsageMetadata(t *testing.T) {
	quotedNotice := "This request was blocked by Gemini's filters. They can occasionally trigger by mistake on safe coding, security, or biology-related queries. Please try rephrasing your prompt. You can [send feedback](https://ai.google.dev/gemini-api/docs/troubleshooting#file-bug) or read more about [our policies here](https://policies.google.com/terms/generative-ai/use-policy)."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"parts":[{"text":"`+quotedNotice+`"}]}}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":40,"totalTokenCount":52}}}`+"\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer server.Close()

	chunks := collectAntigravityStream(t, server.URL, sdktranslator.FormatGemini, sdktranslator.FormatGemini,
		`{"contents":[{"role":"user","parts":[{"text":"explain the filter message"}]}]}`)
	var joined bytes.Buffer
	for _, chunk := range chunks {
		joined.Write(chunk)
	}
	if !bytes.Contains(joined.Bytes(), []byte(quotedNotice)) {
		t.Fatalf("expected quoted filter notice in stream output, got %s", joined.String())
	}
}
