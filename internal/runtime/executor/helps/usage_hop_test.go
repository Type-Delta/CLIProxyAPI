package helps

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/context"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestTrackedHTTPClientRecordsUpstreamHop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	}))
	defer server.Close()

	reporter := NewUsageReporter(context.Background(), "openai", "gpt-test", nil)
	client := reporter.TrackHTTPClient(server.Client())
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions?key=secret#frag", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	record := reporter.buildRecord(usage.Detail{}, true, usage.Failure{StatusCode: 429})
	if record.UpstreamMethod != http.MethodPost || record.UpstreamURL != server.URL+"/v1/chat/completions" {
		t.Fatalf("upstream request = %q %q", record.UpstreamMethod, record.UpstreamURL)
	}
	if record.UpstreamStatusCode != http.StatusTooManyRequests || record.UpstreamSentAt.IsZero() {
		t.Fatalf("upstream response = %d at %v", record.UpstreamStatusCode, record.UpstreamSentAt)
	}
}

func TestParseUsageCapturesRawNode(t *testing.T) {
	openai := ParseOpenAIUsage([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`))
	if openai.RawUsage != `{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}` {
		t.Fatalf("openai raw usage = %q", openai.RawUsage)
	}
	claude := ParseClaudeUsage([]byte(`{"usage":{"input_tokens":2,"output_tokens":4}}`))
	if claude.RawUsage != `{"input_tokens":2,"output_tokens":4}` {
		t.Fatalf("claude raw usage = %q", claude.RawUsage)
	}
	gemini := ParseGeminiUsage([]byte(`{"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2,"totalTokenCount":3}}`))
	if gemini.RawUsage == "" {
		t.Fatal("gemini raw usage missing")
	}
}

// TestStreamUsageCapturesSanitizedGenerationChunk verifies that stream usage
// parsers store the final generation chunk the provider emitted — telemetry
// only, with response content and tool calls stripped before storage.
func TestStreamUsageCapturesSanitizedGenerationChunk(t *testing.T) {
	// OpenAI chat-completions style final chunk: usage plus a content delta.
	openaiDetail, openaiOK := ParseOpenAIStreamUsage([]byte(`data: {"id":"chatcmpl-123","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"content":"secret answer text"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}` + "\n\n"))
	if !openaiOK {
		t.Fatal("openai stream usage not detected")
	}
	if !strings.Contains(openaiDetail.RawUsage, `"prompt_tokens":3`) {
		t.Fatalf("openai raw usage lost usage node: %q", openaiDetail.RawUsage)
	}
	if !strings.Contains(openaiDetail.RawUsage, `"model":"gpt-test"`) || !strings.Contains(openaiDetail.RawUsage, `"id":"chatcmpl-123"`) {
		t.Fatalf("openai raw usage lost generation metadata: %q", openaiDetail.RawUsage)
	}
	if strings.Contains(openaiDetail.RawUsage, "secret") || strings.Contains(openaiDetail.RawUsage, "delta") {
		t.Fatalf("openai raw usage leaked response content: %q", openaiDetail.RawUsage)
	}

	// Claude stream: usage arrives in message_start and message_delta with
	// model/id in the envelope, timing fields on the delta.
	claudeDetail, claudeOK := ParseClaudeStreamUsage([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4,"input_tokens":2}}` + "\n\n"))
	if !claudeOK {
		t.Fatal("claude stream usage not detected")
	}
	if !strings.Contains(claudeDetail.RawUsage, `"type":"message_delta"`) {
		t.Fatalf("claude raw usage lost chunk type: %q", claudeDetail.RawUsage)
	}
	if !strings.Contains(claudeDetail.RawUsage, `"stop_reason":"end_turn"`) {
		t.Fatalf("claude raw usage lost stop telemetry: %q", claudeDetail.RawUsage)
	}
	if strings.Contains(claudeDetail.RawUsage, "secret") {
		t.Fatalf("claude raw usage leaked delta content: %q", claudeDetail.RawUsage)
	}

	// Gemini stream: usageMetadata rides on an interleaved chunk that also
	// carries candidates content.
	geminiDetail, geminiOK := ParseGeminiStreamUsage([]byte(`{"candidates":[{"content":{"parts":[{"text":"secret gemini text"}],"role":"model"}}],"modelVersion":"gemini-test","usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2,"totalTokenCount":3}}`))
	if !geminiOK {
		t.Fatal("gemini stream usage not detected")
	}
	if !strings.Contains(geminiDetail.RawUsage, `"totalTokenCount":3`) {
		t.Fatalf("gemini raw usage lost usage metadata: %q", geminiDetail.RawUsage)
	}
	if !strings.Contains(geminiDetail.RawUsage, `"modelVersion"`) {
		t.Fatalf("gemini raw usage lost model metadata: %q", geminiDetail.RawUsage)
	}
	if strings.Contains(geminiDetail.RawUsage, "secret") || strings.Contains(geminiDetail.RawUsage, `"content"`) {
		t.Fatalf("gemini raw usage leaked response content: %q", geminiDetail.RawUsage)
	}
}
