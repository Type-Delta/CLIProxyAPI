package helps

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestGenerationObservesTokenArrivalsWithoutTerminalDelay(t *testing.T) {
	for _, test := range []struct {
		name                      string
		observe                   func(*UsageReporter, []byte)
		token, terminal, metadata string
	}{
		{"chat", ObserveChatTokenEvent, `{"choices":[{"delta":{"content":"hello"}}]}`, `{"choices":[{"finish_reason":"stop"}]}`, `{"choices":[{"delta":{"role":"assistant"}}]}`},
		{"claude", ObserveClaudeTokenEvent, `{"type":"content_block_delta","delta":{"text":"hello"}}`, `{"type":"message_stop"}`, `{"type":"message_start"}`},
		{"gemini", ObserveGeminiTokenEvent, `{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`, `{"candidates":[{"finishReason":"STOP"}]}`, `{"usageMetadata":{"totalTokenCount":1}}`},
		{"responses", ObserveResponsesTokenEvent, `{"type":"response.output_text.delta","delta":"hello"}`, `{"type":"response.completed"}`, `{"type":"response.created","response":{"created_at":1}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			reporter := NewUsageReporter(context.Background(), test.name, "model", nil)
			reporter.StartResponseTTFT()
			test.observe(reporter, []byte(test.metadata))
			if reporter.generationDuration() != nil {
				t.Fatal("metadata fabricated generation observation")
			}
			test.observe(reporter, []byte(test.token))
			if got := reporter.generationDuration(); got == nil || *got != 0 {
				t.Fatalf("one token duration = %v", got)
			}
			first := reporter.firstTokenAt
			time.Sleep(2 * time.Millisecond)
			test.observe(reporter, []byte(test.token))
			last := reporter.lastTokenAt
			want := last.Sub(first)
			if want <= 0 {
				t.Fatal("second token did not advance generation observation")
			}
			time.Sleep(time.Millisecond)
			test.observe(reporter, []byte(test.terminal))
			record := reporter.buildRecord(usage.Detail{}, false)
			if record.GenerationTime == nil || *record.GenerationTime != want {
				t.Fatalf("duration includes terminal delay: got %v, want %v", record.GenerationTime, want)
			}
			if record.TTFT <= 0 {
				t.Fatal("legacy TTFT was lost")
			}
		})
	}
}

func TestTerminalOnlyResponseDoesNotFabricateGeneration(t *testing.T) {
	reporter := NewUsageReporter(context.Background(), "openai", "model", nil)
	reporter.StartResponseTTFT()
	ObserveResponsesTokenEvent(reporter, []byte(`{"type":"response.completed"}`))
	if reporter.generationDuration() != nil {
		t.Fatal("terminal fallback fabricated generation duration")
	}
	if !reporter.IsTTFTSet() {
		t.Fatal("terminal fallback changed legacy TTFT")
	}
}

func TestGenerationExcludesChatErrorsDoneAndClaudeSignatures(t *testing.T) {
	for _, test := range []struct {
		name, protocol, token, metadata string
		observe                         func(*UsageReporter, []byte)
	}{
		{"chat_error", "openai", `{"choices":[{"delta":{"content":"hello"}}]}`, `{"error":{"message":"stream failed"}}`, ObserveChatTokenEvent},
		{"chat_done", "openai", `{"choices":[{"delta":{"content":"hello"}}]}`, `[DONE]`, ObserveChatTokenEvent},
		{"chat_sse_done", "openai", `{"choices":[{"delta":{"content":"hello"}}]}`, `data: [DONE]`, ObserveChatTokenEvent},
		{"chat_spaced_sse_done", "openai", `{"choices":[{"delta":{"content":"hello"}}]}`, `data:  [DONE]`, ObserveChatTokenEvent},
		{"claude_signature", "claude", `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"hello"}}`, `{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"opaque-signature"}}`, ObserveClaudeTokenEvent},
	} {
		t.Run(test.name, func(t *testing.T) {
			reporter := NewUsageReporter(context.Background(), test.protocol, "model", nil)
			reporter.StartResponseTTFT()
			test.observe(reporter, []byte(test.metadata))
			if reporter.generationDuration() != nil {
				t.Error("metadata-only frame fabricated generation")
			}
			if !reporter.IsTTFTSet() {
				t.Error("legacy TTFT fallback changed")
			}
			reporter = NewUsageReporter(context.Background(), test.protocol, "model", nil)
			reporter.StartResponseTTFT()
			test.observe(reporter, []byte(test.token))
			want := *reporter.generationDuration()
			time.Sleep(time.Millisecond)
			test.observe(reporter, []byte(test.metadata))
			if got := reporter.generationDuration(); got == nil || *got != want {
				t.Errorf("delayed metadata extended generation: got %v, want %v", got, want)
			}
			// Plugin-host dispatch is the live Claude/chat path, and must agree with direct observers.
			pluginReporter := NewUsageReporter(context.Background(), test.protocol, "model", nil)
			pluginReporter.StartResponseTTFT()
			ObservePluginExecutorStreamTTFT(test.protocol, pluginReporter, []byte(test.metadata))
			if pluginReporter.generationDuration() != nil {
				t.Error("plugin metadata-only frame fabricated generation")
			}
		})
	}
}

func TestGenerationTimestampFollowsLockAcquisition(t *testing.T) {
	reporter := NewUsageReporter(context.Background(), "openai", "model", nil)
	reporter.ttftMu.Lock()
	started, finished := make(chan struct{}), make(chan struct{})
	go func() { close(started); reporter.ObserveGenerationToken(); close(finished) }()
	<-started
	// Keep the observer blocked after it enters the method. The timestamp must
	// belong to the serialized observation, not its earlier wait for the lock.
	time.Sleep(10 * time.Millisecond)
	releasedAt := time.Now()
	reporter.ttftMu.Unlock()
	<-finished
	if reporter.firstTokenAt.Before(releasedAt) {
		t.Fatal("generation timestamp was captured before the observation lock")
	}
}
