package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

func jsonRaw(v interface{}) []byte { raw, _ := json.Marshal(v); return raw }

// streamJSON accepts SSE with multiline data, Gemini JSON arrays, and newline
// delimited JSON events. No Scanner token limit truncates large tool arguments.
func streamJSON(body []byte) []gjson.Result {
	trimmed := bytes.TrimSpace(body)
	if json.Valid(trimmed) {
		r := gjson.ParseBytes(trimmed)
		if r.IsArray() {
			return r.Array()
		}
		return []gjson.Result{r}
	}
	var events []gjson.Result
	var data []string
	flush := func() {
		raw := strings.Join(data, "\n")
		if gjson.Valid(raw) {
			events = append(events, gjson.Parse(raw))
		}
		data = nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			// CPA captures may retain data lines while dropping blank separators.
			if len(data) > 0 && gjson.Valid(strings.Join(data, "\n")) {
				flush()
			}
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	if len(events) == 0 {
		d := json.NewDecoder(bytes.NewReader(body))
		for {
			var raw json.RawMessage
			err := d.Decode(&raw)
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			events = append(events, gjson.ParseBytes(raw))
		}
	}
	return events
}

func ParseResponse(f Format, body []byte, streaming bool) (Response, error) {
	out := Response{}
	if !streaming {
		if !gjson.ValidBytes(body) {
			return out, ErrNotTranscript
		}
		return responseJSON(f, gjson.ParseBytes(body))
	}
	events := streamJSON(body)
	if len(events) == 0 {
		return out, ErrNotTranscript
	}
	switch f {
	case FormatOpenAIResponses:
		items := map[int]gjson.Result{}
		for _, ev := range events {
			switch ev.Get("type").String() {
			case "response.output_item.done":
				items[int(ev.Get("output_index").Int())] = ev.Get("item")
			case "response.completed", "response.done", "response.incomplete":
				r, err := responseJSON(f, ev.Get("response"))
				if err == nil {
					return r, nil
				}
				out.ResponseID = ev.Get("response.id").String()
				out.Model = ev.Get("response.model").String()
			case "response.created", "response.in_progress":
				out.ResponseID = ev.Get("response.id").String()
				out.Model = ev.Get("response.model").String()
			}
		}
		for _, i := range sortedKeys(items) {
			if items[i].IsObject() {
				out.Elements = append(out.Elements, element(f, []byte(items[i].Raw), "", false))
			}
		}
	case FormatClaude:
		return claudeStream(events)
	case FormatOpenAIChat:
		return chatStream(events)
	case FormatGemini:
		return geminiStream(events)
	default:
		return out, ErrNotTranscript
	}
	if len(out.Elements) == 0 {
		return out, ErrNotTranscript
	}
	return out, nil
}

func responseJSON(f Format, r gjson.Result) (Response, error) {
	out := Response{ResponseID: r.Get("id").String(), Model: r.Get("model").String()}
	switch f {
	case FormatOpenAIResponses:
		for _, item := range r.Get("output").Array() {
			if item.IsObject() {
				out.Elements = append(out.Elements, element(f, []byte(item.Raw), "", false))
			}
		}
	case FormatOpenAIChat:
		// Alternatives are separate possible conversations. CPA clients normally
		// continue choice zero; including every choice would invent a transcript.
		for _, choice := range r.Get("choices").Array() {
			if choice.Get("index").Int() != 0 {
				continue
			}
			m := choice.Get("message")
			if m.IsObject() {
				out.Elements = append(out.Elements, element(f, []byte(m.Raw), "assistant", false))
				break
			}
		}
	case FormatClaude:
		if r.Get("content").IsArray() && (r.Get("type").String() == "message" || r.Get("role").String() == "assistant") {
			raw := []byte(`{"role":"assistant","content":` + r.Get("content").Raw + `}`)
			out.Elements = append(out.Elements, element(f, raw, "assistant", false))
		}
	case FormatGemini:
		if r.Get("response").IsObject() {
			r = r.Get("response")
		}
		out.ResponseID = r.Get("responseId").String()
		out.Model = r.Get("modelVersion").String()
		for _, c := range r.Get("candidates").Array() {
			if c.Get("index").Int() != 0 {
				continue
			}
			content := c.Get("content")
			if content.IsObject() {
				raw := []byte(content.Raw)
				if !content.Get("role").Exists() {
					var m map[string]interface{}
					_ = json.Unmarshal(raw, &m)
					m["role"] = "model"
					raw = jsonRaw(m)
				}
				out.Elements = append(out.Elements, element(f, raw, "model", false))
				break
			}
		}
	}
	if len(out.Elements) == 0 {
		return out, ErrNotTranscript
	}
	return out, nil
}

func sortedKeys[T any](m map[int]T) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

func claudeStream(events []gjson.Result) (Response, error) {
	out := Response{}
	blocks := map[int]map[string]interface{}{}
	partial := map[int]string{}
	for _, ev := range events {
		i := int(ev.Get("index").Int())
		switch ev.Get("type").String() {
		case "message_start":
			out.ResponseID = ev.Get("message.id").String()
			out.Model = ev.Get("message.model").String()
			for j, b := range ev.Get("message.content").Array() {
				if m, ok := decode([]byte(b.Raw)).(map[string]interface{}); ok {
					blocks[j] = m
				}
			}
		case "content_block_start":
			if m, ok := decode([]byte(ev.Get("content_block").Raw)).(map[string]interface{}); ok {
				blocks[i] = m
			}
		case "content_block_delta":
			b := blocks[i]
			if b == nil {
				continue
			}
			d := ev.Get("delta")
			key := ""
			value := ""
			switch d.Get("type").String() {
			case "text_delta":
				key = "text"
				value = d.Get("text").String()
			case "thinking_delta":
				key = "thinking"
				value = d.Get("thinking").String()
			case "signature_delta":
				key = "signature"
				value = d.Get("signature").String()
			case "input_json_delta":
				partial[i] += d.Get("partial_json").String()
			}
			if key != "" {
				old, _ := b[key].(string)
				b[key] = old + value
			}
		}
	}
	content := make([]interface{}, 0, len(blocks))
	for _, i := range sortedKeys(blocks) {
		if raw, ok := partial[i]; ok {
			if !json.Valid([]byte(raw)) {
				return out, fmt.Errorf("transcript: incomplete Claude tool input")
			}
			blocks[i]["input"] = decode([]byte(raw))
		}
		content = append(content, blocks[i])
	}
	if len(content) == 0 {
		return out, ErrNotTranscript
	}
	out.Elements = []Element{element(FormatClaude, jsonRaw(map[string]interface{}{"role": "assistant", "content": content}), "assistant", false)}
	return out, nil
}

func chatStream(events []gjson.Result) (Response, error) {
	out := Response{}
	m := map[string]interface{}{"role": "assistant"}
	calls := map[int]map[string]interface{}{}
	seen := false
	for _, ev := range events {
		if id := ev.Get("id").String(); id != "" {
			out.ResponseID = id
		}
		if model := ev.Get("model").String(); model != "" {
			out.Model = model
		}
		for _, c := range ev.Get("choices").Array() {
			if c.Get("index").Int() != 0 {
				continue
			}
			d := c.Get("delta")
			if !d.IsObject() {
				continue
			}
			seen = true
			for _, key := range []string{"content", "reasoning_content", "refusal"} {
				if v := d.Get(key); v.Type == gjson.String {
					old, _ := m[key].(string)
					m[key] = old + v.String()
				}
			}
			for _, tc := range d.Get("tool_calls").Array() {
				i := int(tc.Get("index").Int())
				call := calls[i]
				if call == nil {
					call = map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "", "arguments": ""}}
					calls[i] = call
				}
				if id := tc.Get("id").String(); id != "" {
					call["id"] = id
				}
				fun := call["function"].(map[string]interface{})
				for _, key := range []string{"name", "arguments"} {
					if v := tc.Get("function." + key); v.Type == gjson.String {
						fun[key] = fun[key].(string) + v.String()
					}
				}
			}
			if d.Get("function_call").IsObject() {
				fun, ok := m["function_call"].(map[string]interface{})
				if !ok {
					fun = map[string]interface{}{"name": "", "arguments": ""}
					m["function_call"] = fun
				}
				for _, key := range []string{"name", "arguments"} {
					fun[key] = fun[key].(string) + d.Get("function_call."+key).String()
				}
			}
		}
	}
	if !seen {
		return out, ErrNotTranscript
	}
	if len(calls) > 0 {
		a := make([]interface{}, 0, len(calls))
		for _, i := range sortedKeys(calls) {
			a = append(a, calls[i])
		}
		m["tool_calls"] = a
	}
	out.Elements = []Element{element(FormatOpenAIChat, jsonRaw(m), "assistant", false)}
	return out, nil
}

func geminiStream(events []gjson.Result) (Response, error) {
	out := Response{}
	parts := []interface{}{}
	for _, ev := range events {
		if ev.Get("response").IsObject() {
			ev = ev.Get("response")
		}
		if id := ev.Get("responseId").String(); id != "" {
			out.ResponseID = id
		}
		if m := ev.Get("modelVersion").String(); m != "" {
			out.Model = m
		}
		for _, c := range ev.Get("candidates").Array() {
			if c.Get("index").Int() != 0 {
				continue
			}
			for _, p := range c.Get("content.parts").Array() {
				part, ok := decode([]byte(p.Raw)).(map[string]interface{})
				if !ok {
					continue
				}
				// Gemini streams split a single text part across chunks. Merge only
				// adjacent text with matching thought flags, retaining signature metadata.
				if len(parts) > 0 {
					last := parts[len(parts)-1].(map[string]interface{})
					a, aok := last["text"].(string)
					b, bok := part["text"].(string)
					if aok && bok && sameThought(last, part) && onlyTextPart(last) && onlyTextPart(part) {
						last["text"] = a + b
						for _, k := range []string{"thoughtSignature", "thought_signature"} {
							if v, ok := part[k]; ok {
								last[k] = v
							}
						}
						continue
					}
				}
				parts = append(parts, part)
			}
		}
	}
	if len(parts) == 0 {
		return out, ErrNotTranscript
	}
	out.Elements = []Element{element(FormatGemini, jsonRaw(map[string]interface{}{"role": "model", "parts": parts}), "model", false)}
	return out, nil
}
func onlyTextPart(m map[string]interface{}) bool {
	for k := range m {
		if k != "text" && k != "thought" && k != "thoughtSignature" && k != "thought_signature" {
			return false
		}
	}
	return true
}

func sameThought(a, b map[string]interface{}) bool {
	av, aok := a["thought"].(bool)
	bv, bok := b["thought"].(bool)
	return av == bv && aok == bok
}
