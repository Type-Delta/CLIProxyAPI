package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/wsrelay"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"net/http/httptest"
)

type partialUsageTap func(usage.Record)

func (f partialUsageTap) HandleUsage(_ context.Context, r usage.Record) { f(r) }

type resetStreamBody struct{ *strings.Reader }

func (b resetStreamBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		return n, errors.New("upstream read reset")
	}
	return n, err
}
func (b resetStreamBody) Close() error { return nil }

func assertPartialFailureUsage(t *testing.T, records <-chan usage.Record, chunks <-chan cliproxyexecutor.StreamChunk) {
	t.Helper()
	var streamErr error
	for chunk := range chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("expected upstream read failure")
	}
	select {
	case r := <-records:
		if r.Outcome != "failed" || r.Detail.InputTokens != 5 || r.Detail.OutputTokens != 7 || r.Detail.TotalTokens != 12 {
			t.Fatalf("outcome=%s input=%d output=%d total=%d; want failed, 5/7/12", r.Outcome, r.Detail.InputTokens, r.Detail.OutputTokens, r.Detail.TotalTokens)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no usage")
	}
}

func TestStreamReadErrorRetainsObservedUsage(t *testing.T) {
	for _, mode := range []string{"vertex_api_key", "vertex_service_account", "vertex_interactions", "xai"} {
		t.Run(mode, func(t *testing.T) {
			records := make(chan usage.Record, 4)
			unregister, err := usage.RegisterSanitizerTapNamedPlugin(t.Name(), partialUsageTap(func(r usage.Record) { records <- r }))
			if err != nil {
				t.Fatal(err)
			}
			defer unregister(context.Background())
			frame := `{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":7,"totalTokenCount":12}}`
			format := sdktranslator.FormatGemini
			payload := `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`
			model := "gemini-3.7-flash"
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "vertex", Attributes: map[string]string{"api_key": "test-key", "base_url": "http://upstream.invalid"}}
			var exec issue6258StreamExecutor = NewGeminiVertexExecutor(&config.Config{})
			switch mode {
			case "vertex_service_account":
				var serviceAccount map[string]any
				if err := json.Unmarshal(testVertexServiceAccountJSON(t, "http://upstream.invalid/token"), &serviceAccount); err != nil {
					t.Fatal(err)
				}
				auth.Attributes = nil
				auth.Metadata = map[string]any{"project_id": "proxy-test", "location": "global", "service_account": serviceAccount}
			case "vertex_interactions":
				auth.Attributes["interactions"] = "true"
				format = sdktranslator.FormatInteractions
				payload = `{"input":"hello"}`
				frame = `{"type":"interaction.completed","interaction":{"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}}}`
			case "xai":
				auth.Provider = "xai"
				model = "grok-4.3"
				exec = NewXAIExecutor(&config.Config{})
				format = sdktranslator.FormatOpenAIResponse
				payload = `{"input":"hello"}`
				frame = `{"type":"response.completed","response":{"id":"resp-partial","output":[],"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}}}`
			}
			transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/token" {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"access_token":"local-token","token_type":"Bearer","expires_in":3600}`))}, nil
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: resetStreamBody{strings.NewReader("data: " + frame + "\n\n")}}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
			result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: model, Payload: []byte(payload)}, cliproxyexecutor.Options{SourceFormat: format, ResponseFormat: format, Stream: true})
			if err != nil {
				t.Fatal(err)
			}
			assertPartialFailureUsage(t, records, result.Chunks)
		})
	}
}

func TestAIStudioStreamErrorRetainsObservedUsage(t *testing.T) {
	records := make(chan usage.Record, 4)
	unregister, err := usage.RegisterSanitizerTapNamedPlugin(t.Name(), partialUsageTap(func(r usage.Record) { records <- r }))
	if err != nil {
		t.Fatal(err)
	}
	defer unregister(context.Background())
	connected := make(chan struct{})
	relay := wsrelay.NewManager(wsrelay.Options{ProviderFactory: func(*http.Request) (string, error) { return t.Name(), nil }, OnConnected: func(string) { close(connected) }})
	server := httptest.NewServer(relay.Handler())
	defer server.Close()
	defer func() {
		if err := relay.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+relay.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()
	<-connected
	clientDone := make(chan error, 1)
	go func() {
		var request wsrelay.Message
		if err := conn.ReadJSON(&request); err != nil {
			clientDone <- err
			return
		}
		for _, msg := range []wsrelay.Message{
			{ID: request.ID, Type: wsrelay.MessageTypeStreamStart, Payload: map[string]any{"status": 200}},
			{ID: request.ID, Type: wsrelay.MessageTypeStreamChunk, Payload: map[string]any{"data": "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":7,\"totalTokenCount\":12}}\n\n"}},
			{ID: request.ID, Type: wsrelay.MessageTypeError, Payload: map[string]any{"error": "upstream read reset"}},
		} {
			if err := conn.WriteJSON(msg); err != nil {
				clientDone <- err
				return
			}
		}
		clientDone <- nil
	}()
	result, err := NewAIStudioExecutor(&config.Config{}, "aistudio", relay).ExecuteStream(t.Context(), &cliproxyauth.Auth{ID: t.Name(), Provider: "aistudio"}, cliproxyexecutor.Request{Model: "gemini-3.7-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FormatGemini, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	assertPartialFailureUsage(t, records, result.Chunks)
	if err := <-clientDone; err != nil {
		t.Fatal(err)
	}
}
