// Package transcript extracts content-addressed conversation elements without I/O.
package transcript

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

type Format string

const (
	FormatUnknown         Format = ""
	FormatOpenAIChat      Format = "openai-chat"
	FormatOpenAIResponses Format = "openai-responses"
	FormatClaude          Format = "claude"
	FormatGemini          Format = "gemini"
)

var ErrNotTranscript = errors.New("transcript: not a transcript")

type Element struct {
	Raw         []byte
	ContentID   string
	SemanticID  string
	Role        string
	ItemType    string
	BlockTypes  []string
	ToolCallIDs []string
	ToolNames   []string
	Text        string
}
type Request struct {
	Format             Format
	Envelope           []byte
	EnvelopeID         string
	Elements           []Element
	Model              string
	Stream             bool
	PreviousResponseID string
}
type Response struct {
	Elements   []Element
	ResponseID string
	Model      string
}

// DetectFormat prefers explicit wire shape, since CPA accepts Responses input on
// chat routes. A route identifies a protocol even for an error or malformed body.
func DetectFormat(urlPath string, body []byte) Format {
	if u, err := url.Parse(urlPath); err == nil {
		urlPath = u.Path
	}
	// These APIs use different schemas even when they contain an input field.
	for _, part := range []string{"/interactions", "/embeddings", "/audio/", "/images/", "/videos", "/realtime", "/live", "/tts", "/alpha/search"} {
		if strings.Contains(urlPath, part) {
			return FormatUnknown
		}
	}
	r := gjson.ParseBytes(body)
	if r.Get("request.contents").IsArray() || r.Get("request.systemInstruction").Exists() || r.Get("response.candidates").IsArray() || r.Get("contents").IsArray() || r.Get("systemInstruction").Exists() || r.Get("candidates").IsArray() {
		return FormatGemini
	}
	if r.Get("input").Exists() || r.Get("instructions").Exists() || r.Get("previous_response_id").Exists() || r.Get("object").String() == "response" {
		return FormatOpenAIResponses
	}
	if r.Get("messages").IsArray() {
		if r.Get("system").Exists() || r.Get("anthropic_version").Exists() || strings.Contains(urlPath, "/messages") {
			return FormatClaude
		}
		return FormatOpenAIChat
	}
	p := strings.TrimRight(urlPath, "/")
	switch {
	case strings.HasSuffix(p, "/chat/completions"):
		return FormatOpenAIChat
	case strings.HasSuffix(p, "/responses"), strings.HasSuffix(p, "/responses/compact"):
		return FormatOpenAIResponses
	case strings.HasSuffix(p, "/messages"), strings.HasSuffix(p, "/messages/count_tokens"):
		return FormatClaude
	case strings.HasSuffix(p, ":generateContent"), strings.HasSuffix(p, ":streamGenerateContent"), strings.HasSuffix(p, ":countTokens"):
		return FormatGemini
	}
	return FormatUnknown
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:16]) }
func RootEntryID(actorKeyID string, f Format) string {
	return digest([]byte("cpa-transcript-v2\x00" + actorKeyID + "\x00" + string(f)))
}
func EntryID(parentEntryID, semanticID string) string {
	return digest([]byte("cpa-transcript-v2/chain\x00" + parentEntryID + "\x00" + semanticID))
}
func TailStart(elements []Element) int {
	for i := len(elements) - 1; i >= 0; i-- {
		if elements[i].Role == "assistant" || elements[i].Role == "model" {
			return i + 1
		}
	}
	return 0
}

func fields(f Format, body []byte) []string {
	switch f {
	case FormatOpenAIChat:
		return []string{"messages"}
	case FormatClaude:
		return []string{"system", "messages"}
	case FormatOpenAIResponses:
		return []string{"instructions", "input"}
	case FormatGemini:
		if gjson.GetBytes(body, "request").IsObject() {
			return []string{"request.systemInstruction", "request.contents"}
		}
		return []string{"systemInstruction", "contents"}
	}
	return nil
}

type replacement struct {
	start, end int
	raw        []byte
}

func replace(body []byte, changes []replacement) []byte {
	sort.Slice(changes, func(i, j int) bool { return changes[i].start > changes[j].start })
	out := bytes.Clone(body)
	for _, c := range changes {
		out = append(append(append([]byte{}, out[:c.start]...), c.raw...), out[c.end:]...)
	}
	return out
}

// Placeholders carry cardinality and scalar/array shape. The rest of the body is
// left byte-for-byte intact, including property order and number spelling.
const marker = "$cpa_transcript_v1"

func ParseRequest(f Format, body []byte) (Request, error) {
	out := Request{Format: f}
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return out, ErrNotTranscript
	}
	var changes []replacement
	for _, path := range fields(f, body) {
		value := gjson.GetBytes(body, path)
		if !value.Exists() || value.Type == gjson.Null {
			continue
		}
		array := value.IsArray()
		if path == "instructions" && value.Type != gjson.String || path == "system" && !array && value.Type != gjson.String {
			return out, ErrNotTranscript
		}
		pseudo := strings.HasSuffix(path, "system") || strings.HasSuffix(path, "instructions") || strings.HasSuffix(path, "systemInstruction")
		if !array && !pseudo && !(f == FormatOpenAIResponses && value.Type == gjson.String) {
			return out, ErrNotTranscript
		}
		if !array && value.Type != gjson.String && !value.IsObject() {
			return out, ErrNotTranscript
		}
		vals := []gjson.Result{value}
		if array {
			vals = value.Array()
		}
		for _, v := range vals {
			if !v.IsObject() && (v.Type != gjson.String || array && !pseudo) {
				return Request{Format: f}, ErrNotTranscript
			}
			role := ""
			if pseudo {
				role = "system"
			} else if f == FormatOpenAIResponses && v.Type == gjson.String {
				role = "user"
			}
			out.Elements = append(out.Elements, element(f, []byte(v.Raw), role, pseudo))
		}
		placeholder := []byte(fmt.Sprintf(`{"%s":{"count":%d,"array":%t}}`, marker, len(vals), array))
		changes = append(changes, replacement{value.Index, value.Index + len(value.Raw), placeholder})
	}
	if len(changes) == 0 {
		return out, ErrNotTranscript
	}
	out.Envelope = replace(body, changes)
	out.EnvelopeID = digest(out.Envelope)
	out.Model = gjson.GetBytes(body, "model").String()
	out.Stream = gjson.GetBytes(body, "stream").Bool()
	if f == FormatGemini && out.Model == "" {
		out.Model = gjson.GetBytes(body, "request.model").String()
	}
	out.PreviousResponseID = gjson.GetBytes(body, "previous_response_id").String()
	return out, nil
}

func Rebuild(f Format, envelope []byte, raws [][]byte) ([]byte, error) {
	if !gjson.ValidBytes(envelope) {
		return nil, ErrNotTranscript
	}
	var changes []replacement
	cursor := 0
	for _, path := range fields(f, envelope) {
		v := gjson.GetBytes(envelope, path)
		m := v.Get(marker)
		if !m.Exists() {
			continue
		}
		n := m.Get("count").Int()
		if n < 0 || n > int64(len(raws)-cursor) || (!m.Get("array").Bool() && n != 1) {
			return nil, fmt.Errorf("transcript: invalid placeholder cardinality")
		}
		selected := raws[cursor : cursor+int(n)]
		cursor += int(n)
		for _, raw := range selected {
			if !json.Valid(raw) {
				return nil, fmt.Errorf("transcript: invalid element JSON")
			}
		}
		var raw []byte
		if m.Get("array").Bool() {
			raw = append([]byte{'['}, bytes.Join(selected, []byte(","))...)
			raw = append(raw, ']')
		} else {
			raw = selected[0]
		}
		changes = append(changes, replacement{v.Index, v.Index + len(v.Raw), raw})
	}
	if len(changes) == 0 {
		return nil, ErrNotTranscript
	}
	if cursor != len(raws) {
		return nil, fmt.Errorf("transcript: unused elements")
	}
	return replace(envelope, changes), nil
}

func decode(raw []byte) interface{} {
	var v interface{}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	_ = d.Decode(&v)
	return v
}

// volatileSemanticField recognizes replay annotations at protocol boundaries.
// IDs that identify calls or references remain semantic; opaque user payloads
// are never inspected by this function.
func volatileSemanticField(x map[string]interface{}, key, parent string) bool {
	switch key {
	case "status", "cache_control", "signature", "thoughtSignature", "thought_signature", "encrypted_content",
		"caller", "metadata", "provider_metadata", "provider_added", "new_metadata", "request_id", "response_id",
		"namespace", "phase", "annotations", "logprobs", "output_config", "internal_chat_message_metadata_passthrough":
		return true
	case "id":
		typ, _ := x["type"].(string)
		callID := strings.HasSuffix(typ, "tool_use") || typ == "function" ||
			strings.HasSuffix(typ, "_call") && x["call_id"] == nil ||
			typ == "item_reference" || parent == "functionCall" || parent == "functionResponse"
		return !callID
	}
	return false
}

// conservativeSemantic keeps unknown wire content, removing only recognized
// volatile annotations. Prefer an extra occurrence over merging distinct data.
func conservativeSemantic(v interface{}, parent string) interface{} {
	switch parent {
	case "input", "arguments", "args", "output", "response":
		return v
	}
	switch x := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for key, value := range x {
			if !volatileSemanticField(x, key, parent) {
				out[key] = conservativeSemantic(value, key)
			}
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(x))
		for n, value := range x {
			out[n] = conservativeSemantic(value, parent)
		}
		return out
	}
	return v
}

// retainUnknownSemantic preserves newly introduced payload fields on otherwise
// known types. Explicitly understood fields use their protocol normalization;
// only known replay annotations may disappear without inspecting their value.
func retainUnknownSemantic(x, out map[string]interface{}, known map[string]bool, parent string) interface{} {
	for key, value := range x {
		if !known[key] && !volatileSemanticField(x, key, parent) {
			out[key] = conservativeSemantic(value, key)
		}
	}
	return out
}

// normalize selects semantic protocol fields. Tool inputs and outputs are opaque
// user data: never recursively apply protocol field rules inside them. New
// provider metadata is ignored only when it is recognized as replay metadata.
// Unknown types and fields retain their payload through a conservative fallback.
func normalize(v interface{}, f Format, top bool) interface{} {
	switch x := v.(type) {
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, val := range x {
			out[i] = normalize(val, f, false)
		}
		return out
	case map[string]interface{}:
		out := make(map[string]interface{})
		known := make(map[string]bool)
		finish := func() interface{} { return retainUnknownSemantic(x, out, known, "") }
		keep := func(keys ...string) {
			for _, k := range keys {
				known[k] = true
				if val, ok := x[k]; ok && val != nil {
					out[k] = val
				}
			}
		}
		parts := func(keys ...string) {
			for _, k := range keys {
				known[k] = true
				if val, ok := x[k]; ok && val != nil {
					if f == FormatClaude && k == "content" {
						if text, ok := val.(string); ok {
							val = []interface{}{map[string]interface{}{"type": "text", "text": text}}
						}
					}
					out[k] = normalize(val, f, false)
				}
			}
		}
		typ, _ := x["type"].(string)
		if (top && x["role"] != nil && typ == "") || typ == "message" {
			known["type"] = true
			keep("role")
			switch f {
			case FormatOpenAIChat:
				keep("name", "tool_call_id", "reasoning_content", "reasoning", "refusal")
				parts("content", "tool_calls", "function_call", "audio")
			case FormatGemini:
				parts("parts")
			default:
				parts("content")
			}
			return finish()
		}
		if f == FormatGemini {
			// Parts use a discriminating key instead of a type property.
			keep("text", "thought")
			for _, spec := range []struct {
				key    string
				fields []string
			}{
				{"functionCall", []string{"id", "name", "args"}},
				{"functionResponse", []string{"id", "name", "response"}},
				{"inlineData", []string{"mimeType", "data"}},
				{"inline_data", []string{"mime_type", "data"}},
				{"fileData", []string{"mimeType", "fileUri"}},
				{"file_data", []string{"mime_type", "file_uri"}},
				{"executableCode", []string{"language", "code"}},
				{"codeExecutionResult", []string{"outcome", "output"}},
			} {
				if payload, ok := x[spec.key].(map[string]interface{}); ok {
					selected := make(map[string]interface{})
					payloadKnown := make(map[string]bool)
					for _, key := range spec.fields {
						payloadKnown[key] = true
						if value, ok := payload[key]; ok {
							selected[key] = value
						}
					}
					known[spec.key] = true
					out[spec.key] = retainUnknownSemantic(payload, selected, payloadKnown, spec.key)
				}
			}
			// System instructions can omit role.
			parts("parts")
			return finish()
		}
		keep("type")
		switch typ {
		case "text", "input_text", "output_text", "summary_text":
			keep("text")
		case "thinking":
			keep("thinking")
		case "reasoning":
			parts("summary", "content")
		case "redacted_thinking":
			// Only encrypted data exists, with no readable semantics.
		case "tool_use", "server_tool_use":
			keep("id", "name", "input")
		case "tool_result":
			known["content"] = true
			keep("tool_use_id", "is_error")
			if val, ok := x["content"]; ok {
				if _, structured := val.(map[string]interface{}); structured {
					out["content"] = val
				} else {
					parts("content")
				}
			}
		case "function_call":
			keep("call_id", "name", "arguments", "input")
		case "custom_tool_call":
			keep("call_id", "name", "input")
		case "function_call_output", "custom_tool_call_output":
			keep("call_id", "output")
		case "function":
			keep("id")
			parts("function")
		case "image", "document":
			keep("title", "context")
			if source, ok := x["source"].(map[string]interface{}); ok {
				known["source"] = true
				selected := make(map[string]interface{})
				sourceKnown := make(map[string]bool)
				for _, key := range []string{"type", "media_type", "data", "url", "file_id"} {
					sourceKnown[key] = true
					if val, ok := source[key]; ok {
						selected[key] = val
					}
				}
				out["source"] = retainUnknownSemantic(source, selected, sourceKnown, "source")
			}
		case "image_url", "input_image":
			keep("image_url", "file_id", "detail")
		case "input_audio", "audio":
			keep("input_audio", "data", "format", "transcript")
		case "file", "input_file":
			keep("file", "file_id", "file_data", "file_url", "filename")
		case "refusal":
			keep("refusal")
		default:
			if typ != "" {
				return conservativeSemantic(x, "")
			}
			// Untyped Chat function objects and protocol content wrappers.
			keep("name", "arguments", "text")
			parts("content")
		}
		return finish()
	}
	return v
}

func element(f Format, raw []byte, role string, pseudo bool) Element {
	e := Element{Raw: bytes.Clone(raw), ContentID: digest(raw), Role: role, ItemType: "message"}
	v := decode(raw)
	r := gjson.ParseBytes(raw)
	if e.Role == "" {
		e.Role = r.Get("role").String()
	}
	if typ := r.Get("type").String(); typ != "" {
		e.ItemType = typ
	}
	if pseudo {
		e.ItemType = "system"
	}
	if e.Role == "" && f == FormatOpenAIResponses {
		if strings.HasSuffix(e.ItemType, "_call_output") {
			e.Role = "tool"
		} else {
			e.Role = "assistant"
		}
	}
	semantic := normalize(decode(raw), f, true)
	canonical, _ := json.Marshal(struct {
		Role    string      `json:"role"`
		Content interface{} `json:"content"`
	}{e.Role, semantic})
	if hasSemanticContent(semantic) {
		e.SemanticID = digest(canonical)
	}
	var texts []string
	var walk func(interface{}, string)
	walk = func(v interface{}, parent string) {
		switch x := v.(type) {
		case map[string]interface{}:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, key := range []string{"functionCall", "functionResponse", "inlineData", "inline_data", "fileData", "file_data"} {
				if _, ok := x[key]; ok {
					e.BlockTypes = unique(e.BlockTypes, key)
				}
			}
			if _, ok := x["text"]; ok && parent == "parts" {
				e.BlockTypes = unique(e.BlockTypes, "text")
			}
			if typ, ok := x["type"].(string); ok && (parent != "" || typ != "message") {
				e.BlockTypes = unique(e.BlockTypes, typ)
			}
			for _, k := range keys {
				val := x[k]
				if s, ok := val.(string); ok {
					if (k == "id" && (parent == "functionCall" || parent == "functionResponse")) || k == "tool_use_id" || k == "tool_call_id" || k == "call_id" || (k == "id" && (x["type"] == "tool_use" || x["type"] == "server_tool_use" || x["type"] == "mcp_tool_use" || x["type"] == "function")) {
						e.ToolCallIDs = unique(e.ToolCallIDs, s)
					}
					if k == "name" && (x["type"] == "tool_use" || x["type"] == "server_tool_use" || x["type"] == "mcp_tool_use" || x["type"] == "function_call" || x["type"] == "custom_tool_call" || parent == "function" || parent == "functionCall" || parent == "functionResponse" || parent == "function_call") {
						e.ToolNames = unique(e.ToolNames, s)
					}
				}
				if k == "signature" || k == "encrypted_content" || k == "thoughtSignature" || k == "thought_signature" {
					continue
				}
				if k == "data" || k == "file_data" {
					if data, ok := val.(string); ok && (strings.HasPrefix(data, "data:") || base64Characters(data) && (parent == "source" || parent == "inlineData" || parent == "inline_data" || k == "file_data" || len(data) >= 128 || strings.HasSuffix(data, "="))) {
						continue
					}
				}
				walk(val, k)
			}
		case []interface{}:
			for _, val := range x {
				walk(val, parent)
			}
		case string:
			if strings.HasPrefix(x, "data:") || largeBase64(x) {
				return
			}
			texts = append(texts, x)
		}
	}
	walk(v, "")
	e.Text = strings.Join(texts, "\n")
	// Chat tool-call IDs are siblings of the function object.
	for _, tc := range r.Get("tool_calls").Array() {
		e.ToolCallIDs = unique(e.ToolCallIDs, tc.Get("id").String())
	}
	if e.ItemType == "function_call" || e.ItemType == "custom_tool_call" {
		e.ToolNames = unique(e.ToolNames, r.Get("name").String())
	}
	return e
}
func unique(a []string, s string) []string {
	if s == "" {
		return a
	}
	for _, v := range a {
		if v == s {
			return a
		}
	}
	return append(a, s)
}
func base64Characters(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func largeBase64(s string) bool {
	if len(s) <= 64*1024 {
		return false
	}
	return base64Characters(s)
}
