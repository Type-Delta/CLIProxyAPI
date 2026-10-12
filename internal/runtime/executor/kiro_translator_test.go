package executor

import (
	"encoding/json"
	"fmt"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestNormalizeKiroModelID(t *testing.T) {
	for input, want := range map[string]string{
		"claude-haiku-4-5-20251001":  "claude-haiku-4.5",
		"claude-sonnet-4-5":          "claude-sonnet-4.5",
		"claude-opus-4.6":            "claude-opus-4.6",
		"claude-sonnet-4-5-thinking": "claude-sonnet-4.5",
	} {
		if got := NormalizeKiroModelID(input); got != want {
			t.Errorf("NormalizeKiroModelID(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestBuildKiroPayloadConvertsClaudeMessages(t *testing.T) {
	body := []byte(`{"system":"Follow the user","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"result"}]}],"tools":[{"name":"lookup","description":"Lookup","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]}`)
	payload, err := buildKiroPayload("claude-sonnet-4-5-20250929", body, "arn:profile", "conversation-1")
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["profileArn"] != "arn:profile" {
		t.Fatalf("profileArn = %#v", decoded["profileArn"])
	}
	current := decoded["conversationState"].(map[string]any)["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	if current["modelId"] != "claude-sonnet-4.5" {
		t.Fatalf("modelId = %#v", current["modelId"])
	}
	if current["content"] != "(empty placeholder)" {
		t.Fatalf("current content = %#v", current["content"])
	}
	history := decoded["conversationState"].(map[string]any)["history"].([]any)
	if len(history) < 2 {
		t.Fatalf("history = %#v", history)
	}
	if _, ok := current["userInputMessageContext"]; !ok {
		t.Fatal("current tool results missing")
	}
}

func TestBuildKiroPayloadNormalizesTurnsLikeGateway(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":"context"},{"role":"user","content":"one"},{"role":"user","content":"two"}]}`)
	payload, err := buildKiroPayload("claude-sonnet-4-6", body, "", "conversation")
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	state := decoded["conversationState"].(map[string]any)
	history := state["history"].([]any)
	if len(history) != 2 {
		t.Fatalf("history = %#v", history)
	}
	if got := history[0].(map[string]any)["userInputMessage"].(map[string]any)["content"]; got != "(empty placeholder)" {
		t.Fatalf("synthetic user = %#v", got)
	}
	if got := history[1].(map[string]any)["assistantResponseMessage"].(map[string]any)["content"]; got != "context" {
		t.Fatalf("assistant history = %#v", got)
	}
	current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	if got := current["content"]; got != "one\ntwo" {
		t.Fatalf("merged current content = %#v", got)
	}
}

func TestKiroRequestPayloadConvertsOpenAIResponsesInput(t *testing.T) {
	req := cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: []byte(`{"model":"claude-sonnet-4-6","input":"hello"}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
	translated := kiroRequestPayload(req, opts)
	var payload map[string]any
	if err := json.Unmarshal(translated, &payload); err != nil {
		t.Fatal(err)
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v", payload["messages"])
	}
	message := messages[0].(map[string]any)
	if message["role"] != "user" || message["content"] != "hello" {
		t.Fatalf("message = %#v", message)
	}
	if _, err := buildKiroPayload(req.Model, translated, "", "conversation"); err != nil {
		t.Fatalf("translated Responses request is not Kiro-buildable: %v", err)
	}
}

func TestKiroToolsSanitizeCodexSchemas(t *testing.T) {
	tools := kiroTools([]any{map[string]any{
		"name":        "exec_command",
		"description": "run a command",
		"input_schema": map[string]any{
			"type": "object", "additionalProperties": false, "required": []any{},
			"properties": map[string]any{"cmd": map[string]any{"type": "string", "additionalProperties": false}},
		},
	}})
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", tools)
	}
	schema := tools[0]["toolSpecification"].(map[string]any)["inputSchema"].(map[string]any)["json"].(map[string]any)
	if _, ok := schema["additionalProperties"]; ok {
		t.Fatal("top-level additionalProperties was not removed")
	}
	if _, ok := schema["required"]; ok {
		t.Fatal("empty required was not removed")
	}
	properties := schema["properties"].(map[string]any)
	if _, ok := properties["cmd"].(map[string]any)["additionalProperties"]; ok {
		t.Fatal("nested additionalProperties was not removed")
	}
}

func TestKiroToolsCapsRuntimeToolLimit(t *testing.T) {
	raw := make([]any, maxKiroTools+3)
	for i := range raw {
		raw[i] = map[string]any{"name": fmt.Sprintf("tool_%d", i), "description": "tool", "input_schema": map[string]any{"type": "object"}}
	}
	if got := len(kiroTools(raw)); got != maxKiroTools {
		t.Fatalf("kiroTools() returned %d tools, want %d", got, maxKiroTools)
	}
}

func TestKiroEndpointUsesLegacyHostWithoutProfile(t *testing.T) {
	exec := NewKiroExecutor(nil)
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"region": "eu-west-1"}}
	if got, want := exec.endpoint(auth), "https://q.eu-west-1.amazonaws.com/generateAssistantResponse"; got != want {
		t.Fatalf("endpoint without profile = %q, want %q", got, want)
	}
	auth.Metadata["profile_arn"] = "arn:aws:codewhisperer:eu-west-1:123:profile/example"
	if got, want := exec.endpoint(auth), "https://runtime.eu-west-1.kiro.dev/generateAssistantResponse"; got != want {
		t.Fatalf("endpoint with profile = %q, want %q", got, want)
	}
}
