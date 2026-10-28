package transcript

import (
	"encoding/json"
	"math"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// OccurrenceID anchors globally unique provider calls independently of history.
// Other elements remain chain-scoped. Empty elements have no occurrence, while
// their raw content remains available for exact request reconstruction.
//
// The call-ID set is authoritative: changed result bodies with the same IDs
// merge and occurrence search sees the first observed content. A grouped call
// set and its single-call resend split. Mixed Claude result/non-result content
// stays chain-scoped so appended user text gets a searchable entry. Previously,
// anchoring those messages merged them with plain results and hid that text.
// CPA's deterministic call_ plus 24-hex IDs also stay chain-scoped: they omit
// session identity and would otherwise merge identical translated calls across
// sessions in the upstream view. Identical chains can still share occurrences.
func OccurrenceID(actor string, f Format, parent string, e Element) string {
	if e.SemanticID == "" {
		return ""
	}
	ids := []string{}
	kind := ""
	extraContent := false
	add := func(id, k string) bool {
		if id == "" || kind != "" && kind != k {
			return false
		}
		ids = unique(ids, id)
		kind = k
		return true
	}
	r := gjson.ParseBytes(e.Raw)
	switch f {
	case FormatClaude:
		for _, block := range r.Get("content").Array() {
			switch block.Get("type").String() {
			case "tool_use", "server_tool_use", "mcp_tool_use":
				if !add(block.Get("id").String(), "use") {
					return EntryID(parent, e.SemanticID)
				}
			case "tool_result", "mcp_tool_result":
				if !add(block.Get("tool_use_id").String(), "result") {
					return EntryID(parent, e.SemanticID)
				}
			default:
				if hasSemanticContent(normalize(decode([]byte(block.Raw)), f, false)) {
					extraContent = true
				}
			}
		}
	case FormatOpenAIResponses:
		typ := r.Get("type").String()
		if strings.HasSuffix(typ, "_call_output") {
			add(r.Get("call_id").String(), "result")
		} else if strings.HasSuffix(typ, "_call") {
			add(r.Get("call_id").String(), "use")
		}
	case FormatOpenAIChat:
		if e.Role == "tool" {
			add(r.Get("tool_call_id").String(), "result")
		} else {
			for _, call := range r.Get("tool_calls").Array() {
				if !add(call.Get("id").String(), "use") {
					return EntryID(parent, e.SemanticID)
				}
			}
		}
	case FormatGemini:
		for _, part := range r.Get("parts").Array() {
			for _, spec := range []struct{ key, kind string }{{"functionCall", "use"}, {"functionResponse", "result"}} {
				if call := part.Get(spec.key); call.Exists() {
					if !add(call.Get("id").String(), spec.kind) {
						return EntryID(parent, e.SemanticID)
					}
				}
			}
		}
	}
	if kind == "result" && extraContent {
		return EntryID(parent, e.SemanticID)
	}
	if len(ids) == 0 {
		return EntryID(parent, e.SemanticID)
	}
	for _, id := range ids {
		if !uniqueProviderCallID(id) {
			return EntryID(parent, e.SemanticID)
		}
	}
	sort.Strings(ids)
	canonical, _ := json.Marshal([]any{"cpa-transcript-v2/call", actor, string(f), e.Role, kind, ids})
	return digest(canonical)
}

// Known clients also emit counters such as call_0 and functions.Read:0. Only
// long, diverse alphanumeric suffixes qualify; uncertain IDs keep chain scope.
func uniqueProviderCallID(id string) bool {
	// CPA's OpenAI/Gemini adapter hashes position/name/payload into this exact
	// shape. It is deterministic across sessions, even when its bytes look random.
	if strings.HasPrefix(id, "call_") && len(id) == len("call_")+24 {
		synthesized := true
		for _, c := range id[len("call_"):] {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				synthesized = false
				break
			}
		}
		if synthesized {
			return false
		}
	}
	if strings.Contains(id, ":") {
		return false
	}
	for _, prefix := range []string{"toolu_", "call_", "tool_", "fc_", "ctc_"} {
		if strings.HasPrefix(id, prefix) {
			id = strings.TrimPrefix(id, prefix)
			break
		}
	}
	chars := map[rune]int{}
	length := 0
	letter, digit := false, false
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			letter = true
		case c >= '0' && c <= '9':
			digit = true
		case c == '-' || c == '_':
			continue
		default:
			return false
		}
		chars[c]++
		length++
	}
	entropyBits := float64(length) * math.Log2(float64(length))
	for _, count := range chars {
		entropyBits -= float64(count) * math.Log2(float64(count))
	}
	return length >= 20 && len(chars) >= 8 && letter && digit && entropyBits >= 64
}

func hasSemanticContent(v interface{}) bool {
	switch x := v.(type) {
	case map[string]interface{}:
		if typ, _ := x["type"].(string); typ != "" {
			switch typ {
			case "message", "text", "input_text", "output_text", "summary_text", "thinking", "reasoning", "redacted_thinking":
				// Empty readable-content wrappers carry no occurrence.
			default:
				// A new type can itself represent an action, such as wait.
				return true
			}
		}
		for key, value := range x {
			if key != "role" && key != "type" && hasSemanticContent(value) {
				return true
			}
		}
	case []interface{}:
		for _, value := range x {
			if hasSemanticContent(value) {
				return true
			}
		}
	case string:
		return x != ""
	case nil:
		return false
	default:
		return true
	}
	return false
}
