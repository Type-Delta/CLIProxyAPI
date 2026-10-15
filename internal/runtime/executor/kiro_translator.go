package executor

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

var kiroModelVersion = regexp.MustCompile(`^(claude-(?:opus|sonnet|haiku)-\d+)[._-](\d+)(?:[-_].*)?$`)

// NormalizeKiroModelID converts public Claude model aliases to the dotted IDs
// accepted by runtime.<region>.kiro.dev.
func NormalizeKiroModelID(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return model
	}
	if match := kiroModelVersion.FindStringSubmatch(strings.ToLower(model)); len(match) == 3 {
		return match[1] + "." + match[2]
	}
	return strings.TrimSuffix(model, "-thinking")
}

type kiroPayload struct {
	ConversationState kiroConversationState `json:"conversationState"`
	ProfileARN        string                `json:"profileArn,omitempty"`
}

type kiroConversationState struct {
	ChatTriggerType string           `json:"chatTriggerType"`
	ConversationID  string           `json:"conversationId"`
	History         []map[string]any `json:"history,omitempty"`
	CurrentMessage  map[string]any   `json:"currentMessage"`
}

func buildKiroPayload(model string, body []byte, profileARN string, conversationID string) ([]byte, error) {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("decode Claude request for Kiro: %w", err)
	}
	messages, _ := request["messages"].([]any)
	if len(messages) == 0 {
		return nil, fmt.Errorf("Kiro request has no messages")
	}
	messages = normalizeKiroMessages(messages)
	modelID := NormalizeKiroModelID(model)
	if conversationID == "" {
		conversationID = uuid.NewString()
	}
	systemPrompt := kiroSystemPrompt(request["system"])
	history := make([]map[string]any, 0, len(messages))
	for i := 0; i < len(messages)-1; i++ {
		message, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		converted := kiroHistoryMessage(message, modelID)
		if converted != nil {
			history = append(history, converted...)
		}
	}
	if systemPrompt != "" && len(history) > 0 {
		for _, item := range history {
			if input, ok := item["userInputMessage"].(map[string]any); ok {
				if text, ok := input["content"].(string); ok {
					input["content"] = systemPrompt + "\n\n" + text
				}
				break
			}
		}
	}
	currentRaw, _ := messages[len(messages)-1].(map[string]any)
	current := kiroCurrentMessage(currentRaw, modelID, systemPrompt, len(history) == 0)
	if role, _ := currentRaw["role"].(string); strings.EqualFold(role, "assistant") {
		if converted := kiroHistoryMessage(currentRaw, modelID); converted != nil {
			history = append(history, converted...)
		}
		current = kiroCurrentMessage(map[string]any{"role": "user"}, modelID, "", false)
	}
	if len(history) > 0 && !hasUserHistory(history) {
		history = append([]map[string]any{kiroCurrentMessage(map[string]any{"role": "user"}, modelID, "", false)}, history...)
	}
	if len(history) > 1 {
		history = ensureKiroAlternation(history, modelID)
	}
	if tools := kiroTools(request["tools"]); len(tools) > 0 {
		ctx := current["userInputMessage"].(map[string]any)
		contextValue, _ := ctx["userInputMessageContext"].(map[string]any)
		if contextValue == nil {
			contextValue = map[string]any{}
		}
		contextValue["tools"] = tools
		ctx["userInputMessageContext"] = contextValue
	}
	payload := kiroPayload{ConversationState: kiroConversationState{
		ChatTriggerType: "MANUAL", ConversationID: conversationID, History: history, CurrentMessage: map[string]any{"userInputMessage": current["userInputMessage"]},
	}, ProfileARN: strings.TrimSpace(profileARN)}
	return json.Marshal(payload)
}

// normalizeKiroMessages mirrors Kiro Gateway's pre-wire normalization: roles
// are reduced to user/assistant, adjacent turns are merged, and conversations
// begin with a user turn before the alternating history is serialized.
func normalizeKiroMessages(messages []any) []any {
	if len(messages) == 0 {
		return messages
	}
	normalized := make([]any, 0, len(messages)+1)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		if !strings.EqualFold(role, "assistant") {
			message["role"] = "user"
		} else {
			message["role"] = "assistant"
		}
		if len(normalized) > 0 {
			previous, _ := normalized[len(normalized)-1].(map[string]any)
			if previousRole, _ := previous["role"].(string); previousRole == message["role"] {
				previous["content"] = mergeKiroContent(previous["content"], message["content"])
				continue
			}
		}
		normalized = append(normalized, message)
	}
	if len(normalized) == 0 {
		return normalized
	}
	if first, _ := normalized[0].(map[string]any); first["role"] != "user" {
		normalized = append([]any{map[string]any{"role": "user"}}, normalized...)
	}
	return normalized
}

func mergeKiroContent(left, right any) any {
	if leftText, ok := left.(string); ok {
		if rightText, ok := right.(string); ok {
			return leftText + "\n" + rightText
		}
		left = []any{map[string]any{"type": "text", "text": leftText}}
	}
	if rightText, ok := right.(string); ok {
		right = []any{map[string]any{"type": "text", "text": rightText}}
	}
	leftBlocks, leftOK := left.([]any)
	rightBlocks, rightOK := right.([]any)
	if leftOK && rightOK {
		return append(leftBlocks, rightBlocks...)
	}
	if right != nil {
		return right
	}
	return left
}

func kiroSystemPrompt(raw any) string {
	if text, ok := raw.(string); ok {
		return strings.TrimSpace(text)
	}
	blocks, _ := raw.([]any)
	parts := make([]string, 0, len(blocks))
	for _, item := range blocks {
		if block, ok := item.(map[string]any); ok {
			if text, ok := block["text"].(string); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func kiroHistoryMessage(message map[string]any, modelID string) []map[string]any {
	role, _ := message["role"].(string)
	text, images, toolResults := kiroContent(message["content"])
	if role == "user" {
		input := map[string]any{"content": text, "modelId": modelID, "origin": "AI_EDITOR"}
		if len(images) > 0 {
			input["images"] = images
		}
		if len(toolResults) > 0 {
			input["userInputMessageContext"] = map[string]any{"toolResults": toolResults}
		}
		return []map[string]any{{"userInputMessage": input}}
	}
	if role == "assistant" {
		response := map[string]any{"content": text}
		if uses := kiroToolUses(message["content"]); len(uses) > 0 {
			response["toolUses"] = uses
		}
		return []map[string]any{{"assistantResponseMessage": response}}
	}
	return []map[string]any{{"userInputMessage": map[string]any{"content": text, "modelId": modelID, "origin": "AI_EDITOR"}}}
}

func kiroCurrentMessage(message map[string]any, modelID, system string, prependSystem bool) map[string]any {
	text, images, toolResults := kiroContent(message["content"])
	if prependSystem && system != "" {
		text = system + "\n\n" + text
	}
	input := map[string]any{"content": text, "modelId": modelID, "origin": "AI_EDITOR"}
	if len(images) > 0 {
		input["images"] = images
	}
	contextValue := map[string]any{}
	if len(toolResults) > 0 {
		contextValue["toolResults"] = toolResults
	}
	if len(contextValue) > 0 {
		input["userInputMessageContext"] = contextValue
	}
	return map[string]any{"userInputMessage": input}
}

func kiroContent(raw any) (string, []map[string]any, []map[string]any) {
	if text, ok := raw.(string); ok {
		return text, nil, nil
	}
	blocks, _ := raw.([]any)
	var text strings.Builder
	var images []map[string]any
	var results []map[string]any
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			if value, ok := block["text"].(string); ok {
				text.WriteString(value)
			}
		case "image":
			source, _ := block["source"].(map[string]any)
			data, _ := source["data"].(string)
			media, _ := source["media_type"].(string)
			if data != "" {
				images = append(images, map[string]any{"format": strings.TrimPrefix(media, "image/"), "source": map[string]any{"bytes": data}})
			}
		case "tool_result":
			id, _ := block["tool_use_id"].(string)
			resultText, resultImages, _ := kiroContent(block["content"])
			images = append(images, resultImages...)
			if resultText == "" {
				resultText = "(empty result)"
			}
			status := "success"
			if isError, _ := block["is_error"].(bool); isError {
				status = "error"
			}
			results = append(results, map[string]any{"content": []map[string]string{{"text": resultText}}, "status": status, "toolUseId": id})
		}
	}
	return text.String(), images, results
}

func kiroToolUses(raw any) []map[string]any {
	blocks, _ := raw.([]any)
	var uses []map[string]any
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok || block["type"] != "tool_use" {
			continue
		}
		uses = append(uses, map[string]any{"name": block["name"], "input": block["input"], "toolUseId": block["id"]})
	}
	return uses
}

func kiroTools(raw any) []map[string]any {
	items, _ := raw.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tool["name"].(string)
		if name == "" {
			continue
		}
		description, _ := tool["description"].(string)
		schema := sanitizeKiroSchema(tool["input_schema"])
		if schema == nil {
			schema = map[string]any{}
		}
		out = append(out, map[string]any{"toolSpecification": map[string]any{"name": name, "description": description, "inputSchema": map[string]any{"json": schema}}})
	}
	return out
}

// sanitizeKiroSchema removes JSON Schema fields rejected by the Kiro runtime.
// In particular, Codex tool schemas commonly include additionalProperties and
// empty required arrays, both of which produce REQUEST_BODY_INVALID upstream.
func sanitizeKiroSchema(raw any) any {
	switch value := raw.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			if key == "additionalProperties" {
				continue
			}
			if key == "required" {
				if required, ok := item.([]any); ok && len(required) == 0 {
					continue
				}
			}
			out[key] = sanitizeKiroSchema(item)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for index, item := range value {
			out[index] = sanitizeKiroSchema(item)
		}
		return out
	default:
		return raw
	}
}

func hasUserHistory(history []map[string]any) bool {
	for _, item := range history {
		if _, ok := item["userInputMessage"]; ok {
			return true
		}
	}
	return false
}

func ensureKiroAlternation(history []map[string]any, modelID string) []map[string]any {
	out := make([]map[string]any, 0, len(history)*2)
	lastUser := false
	for _, item := range history {
		_, isUser := item["userInputMessage"]
		if isUser && lastUser {
			out = append(out, map[string]any{"assistantResponseMessage": map[string]any{"content": ""}})
		}
		out = append(out, item)
		lastUser = isUser
	}
	return out
}
