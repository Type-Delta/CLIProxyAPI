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
