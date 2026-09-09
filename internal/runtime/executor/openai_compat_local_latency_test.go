package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type captureCompatLatencyUsage struct{ records chan usage.Record }

func (p *captureCompatLatencyUsage) HandleUsage(_ context.Context, record usage.Record) {
	if record.Model == "compat-latency-test" {
		p.records <- record
	}
}

func TestOpenAICompatStreamCapturesSubstantiveTokenLatency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":2,\"total_tokens\":22}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	plugin := &captureCompatLatencyUsage{records: make(chan usage.Record, 1)}
	unregister, err := usage.RegisterPlugin(plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unregister(context.Background()) }()
	executor := NewOpenAICompatExecutor("qa-local", &config.Config{})
	result, err := executor.ExecuteStream(context.Background(), &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}, cliproxyexecutor.Request{Model: "compat-latency-test", Payload: []byte(`{"model":"compat-latency-test","messages":[{"role":"user","content":"hello"}],"stream":true}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
	select {
	case record := <-plugin.records:
		if record.ProviderLatency == nil || record.FirstTokenLatency == nil || record.GenerationTime == nil {
			t.Fatalf("actual compatible SSE stream lost observations: provider=%v first_token=%v generation=%v", record.ProviderLatency, record.FirstTokenLatency, record.GenerationTime)
		}
		if record.TTFT <= 0 || *record.FirstTokenLatency < record.TTFT || *record.FirstTokenLatency < *record.ProviderLatency {
			t.Fatalf("arrival ordering changed: %+v", record)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("usage missing")
	}
}

func TestNativeProviderStreamsCaptureSubstantiveTokenLatency(t *testing.T) {
	cases := []struct{ name, format, body, frames string }{
		{"claude", "claude", `{"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n"},
		{"gemini", "gemini", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}]}}]}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" world\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":20,\"candidatesTokenCount\":2,\"totalTokenCount\":22}}\n\n"},
	}
	vertex := cases[1]
	vertex.name = "vertex"
	cases = append(cases, vertex)
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.frames)
			}))
			defer server.Close()
			plugin := &captureCompatLatencyUsage{records: make(chan usage.Record, 1)}
			unregister, err := usage.RegisterPlugin(plugin)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = unregister(context.Background()) }()
			var execute func(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
			if test.name == "claude" {
				execute = NewClaudeExecutor(&config.Config{}).ExecuteStream
			} else if test.name == "vertex" {
				execute = NewGeminiVertexExecutor(&config.Config{}).ExecuteStream
			} else {
				execute = NewGeminiExecutor(&config.Config{}).ExecuteStream
			}
			result, err := execute(context.Background(), &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}, cliproxyexecutor.Request{Model: "compat-latency-test", Payload: []byte(test.body)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(test.format), Stream: true})
			if err != nil {
				t.Fatal(err)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
			}
			select {
			case record := <-plugin.records:
				if record.ProviderLatency == nil || record.FirstTokenLatency == nil || record.GenerationTime == nil {
					t.Fatalf("native %s lost observations: provider=%v first_token=%v generation=%v", test.name, record.ProviderLatency, record.FirstTokenLatency, record.GenerationTime)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("usage missing")
			}
		})
	}
}
