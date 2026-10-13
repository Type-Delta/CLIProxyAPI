package executor

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
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

func TestKiroEventAccumulatorAccumulatesToolInputFragments(t *testing.T) {
	accumulator := newKiroEventAccumulator("claude-sonnet-4-6", nil)
	accumulator.consume("toolUseEvent", map[string]any{
		"name":      "exec_command",
		"toolUseId": "toolu_1",
	})
	accumulator.consume("toolUseEvent", map[string]any{
		"name":      "exec_command",
		"toolUseId": "toolu_1",
		"input":     `{"command":"ec`,
	})
	accumulator.consume("toolUseEvent", map[string]any{
		"name":      "exec_command",
		"toolUseId": "toolu_1",
		"input":     `ho hi"}`,
	})
	accumulator.consume("toolUseEvent", map[string]any{
		"name":      "exec_command",
		"toolUseId": "toolu_1",
		"stop":      true,
	})

	var response struct {
		Content []struct {
			Type  string         `json:"type"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(accumulator.claudeResponse("claude-sonnet-4-6"), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Content) != 1 || response.Content[0].Type != "tool_use" {
		t.Fatalf("content = %+v, want one tool_use block", response.Content)
	}
	if got := response.Content[0].Input["command"]; got != "echo hi" {
		t.Fatalf("tool input command = %#v, want %q", got, "echo hi")
	}
}

func TestKiroEventAccumulatorDefaultsMissingToolInputToObject(t *testing.T) {
	accumulator := newKiroEventAccumulator("claude-sonnet-4-6", nil)
	accumulator.consume("toolUseEvent", map[string]any{
		"name":      "list_files",
		"toolUseId": "toolu_2",
		"stop":      true,
	})

	var response struct {
		Content []struct {
			Type  string         `json:"type"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(accumulator.claudeResponse("claude-sonnet-4-6"), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Content) != 1 || response.Content[0].Type != "tool_use" {
		t.Fatalf("content = %+v, want one tool_use block", response.Content)
	}
	if response.Content[0].Input == nil {
		t.Fatal("missing tool input object")
	}
}

func TestKiroEventAccumulatorStreamsToolInputAsJSONDelta(t *testing.T) {
	accumulator := newKiroEventAccumulator("claude-sonnet-4-6", nil)
	accumulator.consume("toolUseEvent", map[string]any{
		"name":      "exec_command",
		"toolUseId": "toolu_stream",
		"input":     `{"command":"echo hi"}`,
		"stop":      true,
	})

	var start, delta, stop map[string]any
	for _, chunk := range accumulator.finishSSE("claude-sonnet-4-6") {
		lines := strings.Split(string(chunk), "\n")
		var eventName string
		for _, line := range lines {
			switch {
			case strings.HasPrefix(line, "event: "):
				eventName = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				var payload map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
					t.Fatalf("decode %s payload: %v", eventName, err)
				}
				switch eventName {
				case "content_block_start":
					start = payload
				case "content_block_delta":
					delta = payload
				case "content_block_stop":
					stop = payload
				}
			}
		}
	}

	if got := start["content_block"].(map[string]any)["input"].(map[string]any); len(got) != 0 {
		t.Fatalf("tool start input = %#v, want empty object", got)
	}
	if got := delta["delta"].(map[string]any)["type"]; got != "input_json_delta" {
		t.Fatalf("delta type = %#v, want input_json_delta", got)
	}
	partialJSON := delta["delta"].(map[string]any)["partial_json"].(string)
	var input map[string]any
	if err := json.Unmarshal([]byte(partialJSON), &input); err != nil {
		t.Fatalf("partial_json = %q is invalid JSON: %v", partialJSON, err)
	}
	if input["command"] != "echo hi" {
		t.Fatalf("partial_json command = %#v, want echo hi", input["command"])
	}
	if stop["type"] != "content_block_stop" {
		t.Fatalf("stop event = %#v, want content_block_stop", stop)
	}
}

func TestKiroEventAccumulatorCreatesValidThinkingAndTextBlockLifecycles(t *testing.T) {
	accumulator := newKiroEventAccumulator("claude-sonnet-4-6", nil)
	chunks := [][]byte{
		accumulator.consume("reasoningContentEvent", map[string]any{"text": "Inspect first"}),
		accumulator.consume("assistantResponseEvent", map[string]any{"content": "Visible answer"}),
		accumulator.consume("reasoningContentEvent", map[string]any{"text": "Inspect first. More reasoning"}),
		accumulator.consume("assistantResponseEvent", map[string]any{"content": "Visible answer. More text"}),
	}
	chunks = append(chunks, accumulator.finishSSE("claude-sonnet-4-6")...)
	events := decodeKiroSSE(t, chunks)

	type key struct {
		event string
		index int
	}
	lifecycle := make([]key, 0, len(events))
	for _, event := range events {
		if !strings.HasPrefix(event.name, "content_block_") {
			continue
		}
		index := int(gjson.GetBytes(event.payload, "index").Int())
		lifecycle = append(lifecycle, key{event.name, index})
	}
	want := []key{
		{"content_block_start", 0}, {"content_block_delta", 0},
		{"content_block_delta", 0}, {"content_block_stop", 0},
	}
	if fmt.Sprint(lifecycle) != fmt.Sprint(want) {
		t.Fatalf("block lifecycle = %v, want %v", lifecycle, want)
	}
	blockTypes := []string{}
	for _, event := range events {
		if event.name == "content_block_start" {
			blockTypes = append(blockTypes, gjson.GetBytes(event.payload, "content_block.type").String())
		}
	}
	if fmt.Sprint(blockTypes) != "[text]" {
		t.Fatalf("block types = %v, want text only because Kiro has no thinking signature", blockTypes)
	}
	if got := gjson.GetBytes(events[1].payload, "delta.type").String(); got != "text_delta" {
		t.Fatalf("first delta type = %q, want text_delta", got)
	}
}

func TestKiroClaudeResponsePreservesBufferedThinking(t *testing.T) {
	tests := []struct {
		name       string
		reasoning  string
		answer     string
		wantText   string
		wantAnswer string
	}{
		{name: "reasoning-only", reasoning: "silent reasoning", wantText: "silent reasoning"},
		{name: "reasoning-and-answer", reasoning: "plan first", answer: "final answer", wantText: "plan first", wantAnswer: "final answer"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accumulator := newKiroEventAccumulator("claude-sonnet-4-6", nil)
			accumulator.consume("reasoningContentEvent", map[string]any{"text": test.reasoning})
			if test.answer != "" {
				accumulator.consume("assistantResponseEvent", map[string]any{"content": test.answer})
			}
			response := accumulator.claudeResponse("claude-sonnet-4-6")

			var decoded struct {
				Content []struct {
					Type     string `json:"type"`
					Thinking string `json:"thinking"`
					Text     string `json:"text"`
				} `json:"content"`
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(response, &decoded); err != nil {
				t.Fatal(err)
			}
			wantCount := 0
			if test.answer != "" {
				wantCount = 1
			}
			if len(decoded.Content) != wantCount {
				t.Fatalf("content = %+v, want %d blocks", decoded.Content, wantCount)
			}
			if test.answer != "" && (decoded.Content[0].Type != "text" || decoded.Content[0].Text != test.wantAnswer) {
				t.Fatalf("answer block = %+v, want %q", decoded.Content[0], test.wantAnswer)
			}
			if decoded.Usage.OutputTokens == 0 {
				t.Fatalf("reasoning-only output tokens = %d, want non-zero", decoded.Usage.OutputTokens)
			}
		})
	}
}

func TestKiroFinalizesPayloadExactlyBeforeTransport(t *testing.T) {
	captured := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read Kiro body: %v", errRead)
			return
		}
		captured <- body
		writeKiroTestEvents(t, w, []kiroTestEvent{{name: "assistantResponseEvent", payload: map[string]any{"content": "ok"}}})
	}))
	t.Cleanup(server.Close)

	cfg := &config.Config{Payload: config.PayloadConfig{
		Filter: []config.PayloadFilterRule{{
			Models: []config.PayloadModelRule{{Name: "claude-sonnet-*"}},
			Params: []string{"profileArn"},
		}},
		Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{Name: "claude-sonnet-*"}},
			Params: map[string]any{"conversationState.chatTriggerType": "CONFIGURED"},
		}},
	}}
	configuredTools := make([]map[string]any, 106)
	for index := range configuredTools {
		configuredTools[index] = map[string]any{
			"toolSpecification": map[string]any{
				"name":        fmt.Sprintf("configured_tool_%d", index),
				"description": "configured",
				"inputSchema": map[string]any{"json": map[string]any{"type": "object"}},
			},
		}
	}
	cfg.Payload.Override[0].Params["conversationState.currentMessage.userInputMessage.userInputMessageContext.tools"] = configuredTools
	executor := NewKiroExecutor(cfg)
	ctx := kiroUsageTestContext(server, t.Name())
	request := cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: true}
	if _, errExecute := executor.Execute(ctx, kiroUsageTestAuth(), request, opts); errExecute != nil {
		t.Fatalf("Execute: %v", errExecute)
	}
	body := <-captured
	if gjson.GetBytes(body, "profileArn").Exists() {
		t.Fatalf("non-stream profileArn survived payload filtering: %s", body)
	}
	if got := gjson.GetBytes(body, "conversationState.chatTriggerType").String(); got != "CONFIGURED" {
		t.Fatalf("non-stream chatTriggerType = %q, want configured override: %s", got, body)
	}
	if got := gjson.GetBytes(body, "conversationState.currentMessage.userInputMessage.userInputMessageContext.tools.#").Int(); got != 106 {
		t.Fatalf("non-stream configured tool count = %d, want 106: %s", got, body)
	}
	if got := gjson.GetBytes(body, "conversationState.currentMessage.userInputMessage.userInputMessageContext.tools.105.toolSpecification.name").String(); got != "configured_tool_105" {
		t.Fatalf("non-stream final tool = %q, want configured_tool_105: %s", got, body)
	}

	result, errStream := executor.ExecuteStream(ctx, kiroUsageTestAuth(), request, opts)
	if errStream != nil {
		t.Fatalf("ExecuteStream: %v", errStream)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}
	body = <-captured
	if gjson.GetBytes(body, "profileArn").Exists() {
		t.Fatalf("stream profileArn survived payload filtering: %s", body)
	}
	if got := gjson.GetBytes(body, "conversationState.chatTriggerType").String(); got != "CONFIGURED" {
		t.Fatalf("stream chatTriggerType = %q, want configured override: %s", got, body)
	}
	if got := gjson.GetBytes(body, "conversationState.currentMessage.userInputMessage.userInputMessageContext.tools.#").Int(); got != 106 {
		t.Fatalf("stream configured tool count = %d, want 106: %s", got, body)
	}
	if got := gjson.GetBytes(body, "conversationState.currentMessage.userInputMessage.userInputMessageContext.tools.105.toolSpecification.name").String(); got != "configured_tool_105" {
		t.Fatalf("stream final tool = %q, want configured_tool_105: %s", got, body)
	}
}

func TestParseKiroEventStreamRejectsIncompleteAndMalformedStreams(t *testing.T) {
	invalidPrelude := make([]byte, 12)
	binary.BigEndian.PutUint32(invalidPrelude[0:4], 8)
	binary.BigEndian.PutUint32(invalidPrelude[8:12], crc32.ChecksumIEEE(invalidPrelude[:8]))
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{name: "empty", body: nil, want: "ended without events"},
		{name: "invalid lengths", body: invalidPrelude, want: "invalid Kiro event frame lengths"},
		{
			name: "malformed JSON",
			body: kiroTestRawEventFrame("assistantResponseEvent", []byte(`{"content":`)),
			want: "invalid Kiro event frame",
		},
		{
			name: "exception",
			body: kiroTestRawEventFrame(
				"ThrottlingException",
				[]byte(`{"message":"rate limited"}`),
				kiroTestStringHeader(":message-type", "exception"),
			),
			want: "Kiro stream exception ThrottlingException",
		},
		{
			name: "error message type",
			body: kiroTestRawEventFrame("", []byte(`{"message":"bad request"}`), kiroTestStringHeader(":message-type", "error"), kiroTestStringHeader(":error-code", "BadRequest")),
			want: "Kiro stream exception",
		},
		{
			name: "trailing malformed header",
			body: kiroTestRawEventFrame("assistantResponseEvent", []byte(`{"content":"ok"}`), []byte{3}),
			want: "invalid Kiro event headers",
		},
		{name: "null payload", body: kiroTestRawEventFrame("assistantResponseEvent", []byte("null")), want: "invalid Kiro event frame"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := parseKiroEventStream(bytes.NewReader(test.body), func(string, map[string]any) bool { return true })
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseKiroEventStream() error = %v, want %q", err, test.want)
			}
		})
	}

	valid := kiroTestEventFrame(t, "assistantResponseEvent", map[string]any{"content": "ok"})
	if err := parseKiroEventStream(bytes.NewReader(valid), func(string, map[string]any) bool { return true }); err != nil {
		t.Fatalf("parseKiroEventStream(valid) error = %v", err)
	}
	validHeader := kiroTestRawEventFrame("assistantResponseEvent", []byte(`{"content":"ok"}`), kiroTestByteHeader("binary", []byte{1, 2, 3}))
	if err := parseKiroEventStream(bytes.NewReader(validHeader), func(string, map[string]any) bool { return true }); err != nil {
		t.Fatalf("parseKiroEventStream(valid byte header) error = %v", err)
	}
	validFixedHeaders := kiroTestRawEventFrame("assistantResponseEvent", []byte(`{"content":"ok"}`), kiroTestFixedHeader("timestamp", 8, 8), kiroTestFixedHeader("uuid", 9, 16))
	if err := parseKiroEventStream(bytes.NewReader(validFixedHeaders), func(string, map[string]any) bool { return true }); err != nil {
		t.Fatalf("parseKiroEventStream(valid timestamp/uuid headers) error = %v", err)
	}
}

func TestParseKiroEventStreamRejectsCorruptCRCAndOversizedFrames(t *testing.T) {
	valid := kiroTestRawEventFrame("assistantResponseEvent", []byte(`{"content":"ok"}`))
	valid[12] ^= 1
	if err := parseKiroEventStream(bytes.NewReader(valid), func(string, map[string]any) bool { return true }); err == nil || !strings.Contains(err.Error(), "CRC") {
		t.Fatalf("corrupt frame error = %v, want CRC error", err)
	}
	oversized := make([]byte, 12)
	binary.BigEndian.PutUint32(oversized[0:4], maxKiroFrameSize+1)
	binary.BigEndian.PutUint32(oversized[4:8], 0)
	binary.BigEndian.PutUint32(oversized[8:12], crc32.ChecksumIEEE(oversized[:8]))
	if err := parseKiroEventStream(bytes.NewReader(oversized), func(string, map[string]any) bool { return true }); err == nil || !strings.Contains(err.Error(), "frame lengths") {
		t.Fatalf("oversized frame error = %v, want frame length error", err)
	}
}

func TestParseKiroEventStreamRejectsEOFBeforeAnEvent(t *testing.T) {
	frame := kiroTestEventFrame(t, "assistantResponseEvent", map[string]any{"content": "partial"})
	truncated := frame[:len(frame)-3]
	err := parseKiroEventStream(bytes.NewReader(truncated), func(string, map[string]any) bool { return true })
	if err == nil || (!errors.Is(err, io.ErrUnexpectedEOF) && !strings.Contains(err.Error(), "unexpected EOF")) {
		t.Fatalf("truncated stream error = %v, want unexpected EOF", err)
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

type decodedKiroSSE struct {
	name    string
	payload []byte
}

func decodeKiroSSE(t *testing.T, chunks [][]byte) []decodedKiroSSE {
	t.Helper()
	events := make([]decodedKiroSSE, 0, len(chunks))
	for _, chunk := range chunks {
		var name string
		for _, line := range strings.Split(string(chunk), "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				payload := []byte(strings.TrimPrefix(line, "data: "))
				if !json.Valid(payload) {
					t.Fatalf("invalid SSE payload %q", payload)
				}
				events = append(events, decodedKiroSSE{name: name, payload: payload})
				name = ""
			}
		}
	}
	return events
}

func kiroTestStringHeader(name, value string) []byte {
	if len(name) > 255 || len(value) > 65535 {
		panic("Kiro test header is too large")
	}
	header := []byte{byte(len(name))}
	header = append(header, name...)
	header = append(header, 7)
	var valueLength [2]byte
	binary.BigEndian.PutUint16(valueLength[:], uint16(len(value)))
	header = append(header, valueLength[:]...)
	return append(header, value...)
}

func kiroTestByteHeader(name string, value []byte) []byte {
	if len(name) > 255 || len(value) > 65535 {
		panic("Kiro test byte header is too large")
	}
	header := []byte{byte(len(name))}
	header = append(header, name...)
	header = append(header, 6)
	var valueLength [2]byte
	binary.BigEndian.PutUint16(valueLength[:], uint16(len(value)))
	header = append(header, valueLength[:]...)
	return append(header, value...)
}

func kiroTestFixedHeader(name string, valueType byte, length int) []byte {
	if len(name) > 255 {
		panic("Kiro test fixed header is too large")
	}
	header := []byte{byte(len(name))}
	header = append(header, name...)
	header = append(header, valueType)
	return append(header, make([]byte, length)...)
}

func kiroTestRawEventFrame(eventName string, payload []byte, extraHeaders ...[]byte) []byte {
	headers := kiroTestStringHeader(":event-type", eventName)
	for _, extra := range extraHeaders {
		headers = append(headers, extra...)
	}
	totalLength := 12 + len(headers) + len(payload) + 4
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	frame = append(frame, headers...)
	frame = append(frame, payload...)
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], crc32.ChecksumIEEE(frame))
	return append(frame, crc[:]...)
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
