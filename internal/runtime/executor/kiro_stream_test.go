package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestKiroEventAccumulatorEstimatesUsageWhenKiroOmitsTokenCounts(t *testing.T) {
	accumulator := newKiroEventAccumulator("claude-sonnet-4-6", []byte(`{"messages":[{"role":"user","content":"Count this request"}]}`))
	if accumulator.inputTokens == 0 {
		t.Fatal("input token estimate is zero")
	}
	accumulator.consume("assistantResponseEvent", map[string]any{"content": "A generated answer"})
	accumulator.consume("meteringEvent", map[string]any{"usage": 1.25, "contextUsagePercentage": 2.5})
	response := accumulator.claudeResponse("claude-sonnet-4-6")
	var decoded struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(response, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Usage.InputTokens == 0 || decoded.Usage.OutputTokens == 0 {
		t.Fatalf("usage = %+v, want non-zero estimates", decoded.Usage)
	}
}

func TestEmitTranslatedKiroSSEUsesDataLines(t *testing.T) {
	out := make(chan cliproxyexecutor.StreamChunk, 2)
	var param any
	ok := emitTranslatedKiroSSE(context.Background(), out, sdktranslator.FormatOpenAI, "claude-sonnet-4-6", nil, nil, []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-6\",\"content\":[],\"usage\":{\"input_tokens\":0}}}\n\n"), &param)
	if !ok {
		t.Fatal("emitTranslatedKiroSSE returned false")
	}
	select {
	case chunk := <-out:
		if !strings.Contains(string(chunk.Payload), `"object":"chat.completion.chunk"`) {
			t.Fatalf("translated chunk = %s", chunk.Payload)
		}
	default:
		t.Fatal("no translated chunk")
	}
}

func TestEmitTranslatedKiroSSEPreservesResponsesStateAcrossEvents(t *testing.T) {
	out := make(chan cliproxyexecutor.StreamChunk, 16)
	var param any
	chunks := []string{
		`event: message_start\ndata: {"type":"message_start","message":{"id":"msg_1","role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input_tokens":0}}}\n\n`,
		`event: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}\n\n`,
		`event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}\n\n`,
		`event: content_block_stop\ndata: {"type":"content_block_stop","index":0}\n\n`,
		`event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}\n\n`,
		`event: message_stop\ndata: {"type":"message_stop"}\n\n`,
	}
	for _, chunk := range chunks {
		if !emitTranslatedKiroSSE(context.Background(), out, sdktranslator.FormatOpenAIResponse, "claude-sonnet-4-6", nil, nil, []byte(strings.ReplaceAll(chunk, `\n`, "\n")), &param) {
			t.Fatal("emitTranslatedKiroSSE returned false")
		}
	}
	var completed string
	for len(out) > 0 {
		chunk := <-out
		if strings.Contains(string(chunk.Payload), "response.completed") {
			completed = string(chunk.Payload)
		}
	}
	if completed == "" || !strings.Contains(completed, `"id":"msg_1"`) || !strings.Contains(completed, `"output"`) {
		t.Fatalf("completed event lost stream state: %s", completed)
	}
}
