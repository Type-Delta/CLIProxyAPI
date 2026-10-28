package transcript

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func mustRequest(t testing.TB, f Format, s string) Request {
	t.Helper()
	r, err := ParseRequest(f, []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func raws(es []Element) [][]byte {
	out := make([][]byte, len(es))
	for i := range es {
		out[i] = es[i].Raw
	}
	return out
}
func sameJSON(t testing.TB, a, b []byte) {
	t.Helper()
	if !reflect.DeepEqual(decode(a), decode(b)) {
		t.Fatalf("JSON differs\n%s\n%s", a, b)
	}
}

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		path, body string
		want       Format
	}{
		{"/v1/chat/completions", `{"messages":[]}`, FormatOpenAIChat},
		{"https://api.example/v1/chat/completions?x=1", `{}`, FormatOpenAIChat},
		{"/v1/chat/completions", `{"input":"hello"}`, FormatOpenAIResponses},
		{"/v1/responses", `{}`, FormatOpenAIResponses}, {"/v1/responses/compact", `{}`, FormatOpenAIResponses},
		{"https://chatgpt.com/backend-api/codex/responses", `{}`, FormatOpenAIResponses},
		{"/backend-api/codex/responses/compact", `{}`, FormatOpenAIResponses},
		{"/v1/messages", `{"messages":[]}`, FormatClaude}, {"https://api.anthropic.com/v1/messages?beta=true", `{}`, FormatClaude},
		{"/v1/messages/count_tokens", `{}`, FormatClaude}, {"", `{"system":"s","messages":[]}`, FormatClaude},
		{"/v1beta/models/m:generateContent", `{}`, FormatGemini}, {"/v1/models/m:streamGenerateContent?alt=sse", `{}`, FormatGemini},
		{"/v1internal:streamGenerateContent", `{"request":{"contents":[]}}`, FormatGemini},
		{"/v1internal:generateContent", `{}`, FormatGemini}, {"/v1beta/models/m:countTokens", `{}`, FormatGemini},
		{"", `{"contents":[]}`, FormatGemini}, {"", `{"instructions":"s","input":[]}`, FormatOpenAIResponses},
		{"/v1beta/interactions", `{"input":"hi"}`, FormatUnknown},
		{"/v1/embeddings", `{"input":"hi"}`, FormatUnknown},
		{"/v1/audio/speech", `{"input":"hi"}`, FormatUnknown},
		{"/v1/images/generations", `{"prompt":"hi"}`, FormatUnknown},
		{"/v1/videos", `{}`, FormatUnknown}, {"/v1/tts", `{}`, FormatUnknown}, {"/v1/realtime", `{}`, FormatUnknown},
		{"/v1/alpha/search", `{}`, FormatUnknown}, {"/v1/live", `{}`, FormatUnknown},
		{"/v1/completions", `{"prompt":"hi"}`, FormatUnknown}, {"/v1/models", `{}`, FormatUnknown},
		{"", `{"surprise":true}`, FormatUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.path+tt.body, func(t *testing.T) {
			if got := DetectFormat(tt.path, []byte(tt.body)); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestRequestRoundTrips(t *testing.T) {
	tests := []struct {
		f     Format
		body  string
		count int
	}{
		{FormatOpenAIChat, `{ "model":"chat", "messages": [ {"role":"system","content":"rules"}, {"role":"user","content":[{"type":"text","text":"ask"}]} ], "tools":[], "temperature":1.00, "stream":true }`, 2},
		{FormatClaude, `{"model":"c","system":"rules","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"read"}]}`, 2},
		{FormatClaude, `{"system":[{"type":"text","text":"rules","cache_control":{"type":"ephemeral"}}],"messages":[],"max_tokens":10}`, 1},
		{FormatOpenAIResponses, `{"instructions":"rules","input":"hello","previous_response_id":"resp_old","model":"codex","store":false}`, 2},
		{FormatOpenAIResponses, `{"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"function_call_output","call_id":"c1","output":"result"}]}`, 2},
		{FormatGemini, `{"systemInstruction":{"parts":[{"text":"rules"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":1e-2}}`, 2},
		{FormatGemini, `{ "project":"p", "model":"g", "request": { "contents": [ {"role":"user","parts":[{"text":"hi"}]} ], "systemInstruction":{"parts":[{"text":"rules"}]}, "tools":[] } }`, 2},
		{FormatOpenAIChat, `{"messages":[],"tools":[]}`, 0},
	}
	for _, tt := range tests {
		t.Run(string(tt.f)+tt.body, func(t *testing.T) {
			r := mustRequest(t, tt.f, tt.body)
			if len(r.Elements) != tt.count {
				t.Fatalf("count %d", len(r.Elements))
			}
			b, err := Rebuild(tt.f, r.Envelope, raws(r.Elements))
			if err != nil {
				t.Fatal(err)
			}
			sameJSON(t, []byte(tt.body), b)
			// Removing whitespace solely inside rebuilt transcript arrays is allowed.
			if !strings.Contains(tt.body, "[ ") && !bytes.Equal(b, []byte(tt.body)) {
				t.Fatalf("bytes changed: %s", b)
			}
			if r.EnvelopeID != digest(r.Envelope) {
				t.Fatal("envelope hash")
			}
			for _, e := range r.Elements {
				if e.ContentID != digest(e.Raw) {
					t.Fatal("content hash")
				}
			}
		})
	}
	r := mustRequest(t, FormatOpenAIResponses, tests[3].body)
	if r.Model != "codex" || r.PreviousResponseID != "resp_old" {
		t.Fatal(r)
	}
	r = mustRequest(t, FormatOpenAIChat, tests[0].body)
	if !r.Stream || r.Model != "chat" {
		t.Fatal(r)
	}
}

func TestConvergenceJSON(t *testing.T) {
	tests := []struct {
		f              Format
		response, next string
	}{
		{FormatClaude, `{"id":"msg1","model":"c","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"consider","signature":"sig-old"},{"type":"text","text":"answer"},{"type":"tool_use","id":"tool1","name":"read","input":{"path":"a.go"}}]}`, `{"messages":[{"role":"assistant","cache_control":{"type":"ephemeral"},"content":[{"signature":"sig-new","thinking":"consider","type":"thinking"},{"text":"answer","type":"text","cache_control":{"type":"ephemeral"}},{"type":"tool_use","id":"tool1","name":"read","input":{"path":"a.go"}}]}]}`},
		{FormatOpenAIResponses, `{"id":"resp1","model":"codex","output":[{"id":"rs1","type":"reasoning","status":"completed","encrypted_content":"old","summary":[{"type":"summary_text","text":"consider"}]},{"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer","annotations":[],"logprobs":[]}]},{"type":"function_call","id":"fc1","call_id":"call1","name":"read","arguments":"{\"path\":\"a.go\"}","status":"completed"}]}`, `{"previous_response_id":"resp0","input":[{"type":"reasoning","encrypted_content":"new","summary":[{"type":"summary_text","text":"consider"}]},{"role":"assistant","content":[{"text":"answer","type":"output_text"}]},{"type":"function_call","call_id":"call1","name":"read","arguments":"{\"path\":\"a.go\"}"}]}`},
		{FormatOpenAIChat, `{"id":"chat1","model":"o","choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":null,"reasoning_content":"consider","tool_calls":[{"id":"call1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a.go\"}"}}]}}]}`, `{"messages":[{"role":"assistant","reasoning_content":"consider","tool_calls":[{"type":"function","id":"call1","function":{"arguments":"{\"path\":\"a.go\"}","name":"read"}}]}]}`},
		{FormatGemini, `{"responseId":"g1","modelVersion":"g","candidates":[{"index":0,"content":{"role":"model","parts":[{"thought":true,"text":"consider","thoughtSignature":"sig1"},{"functionCall":{"name":"read","args":{"path":"a.go"}}}]}}]}`, `{"contents":[{"role":"model","parts":[{"thought":true,"text":"consider","thoughtSignature":"sig2"},{"functionCall":{"args":{"path":"a.go"},"name":"read"}}]}]}`},
	}
	for _, tt := range tests {
		t.Run(string(tt.f), func(t *testing.T) {
			resp, err := ParseResponse(tt.f, []byte(tt.response), false)
			if err != nil {
				t.Fatal(err)
			}
			next := mustRequest(t, tt.f, tt.next)
			if len(resp.Elements) != len(next.Elements) {
				t.Fatal("count")
			}
			for i, e := range resp.Elements {
				if e.SemanticID != next.Elements[i].SemanticID {
					t.Fatalf("convergence failed %s\n%s\n%s", tt.f, e.Raw, next.Elements[i].Raw)
				}
				if e.ContentID == next.Elements[i].ContentID {
					t.Fatal("fixture must exercise normalization")
				}
			}
			if resp.ResponseID == "" || resp.Model == "" {
				t.Fatal("missing response metadata")
			}
		})
	}
}

func sse(events ...string) []byte {
	return []byte("event: synthetic\r\n" + "data: " + strings.Join(events, "\r\n\r\ndata: ") + "\r\n\r\ndata: [DONE]\r\n\r\n")
}
func TestResponseStreams(t *testing.T) {
	tests := []struct {
		f    Format
		body []byte
		next string
	}{
		{FormatClaude, sse(
			`{"type":"message_start","message":{"id":"m","model":"c","content":[]}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"consider"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"secret"}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t","name":"read","input":{}}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"a.go\"}"}}`,
			`{"type":"message_stop"}`), `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"consider","signature":"replayed"},{"type":"text","text":"answer"},{"type":"tool_use","id":"t","name":"read","input":{"path":"a.go"}}]}]}`},
		{FormatOpenAIChat, sse(
			`{"id":"c","model":"o","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"consider","tool_calls":[{"index":1,"id":"b","function":{"name":"second","arguments":"{"}},{"index":0,"id":"a","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"answer","tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}},{"index":1,"function":{"arguments":"}"}}]}}]}`), `{"messages":[{"role":"assistant","reasoning_content":"consider","content":"answer","tool_calls":[{"id":"a","type":"function","function":{"name":"read","arguments":"{\"path\":\"a.go\"}"}},{"id":"b","type":"function","function":{"name":"second","arguments":"{}"}}]}]}`},
		{FormatOpenAIResponses, sse(
			`{"type":"response.created","response":{"id":"r","model":"o"}}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"t","id":"f","name":"read","arguments":"{}"}}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","encrypted_content":"secret","summary":[{"type":"summary_text","text":"consider"}]}}`), `{"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"consider"}]},{"type":"function_call","call_id":"t","name":"read","arguments":"{}"}]}`},
		{FormatGemini, sse(`{"response":{"responseId":"g","modelVersion":"g","candidates":[{"content":{"parts":[{"text":"ans"}]}}]}}`, `{"response":{"candidates":[{"content":{"parts":[{"text":"wer","thoughtSignature":"sig"},{"functionCall":{"name":"read","args":{"path":"a.go"}}}]}}]}}`), `{"contents":[{"role":"model","parts":[{"text":"answer","thoughtSignature":"replay"},{"functionCall":{"name":"read","args":{"path":"a.go"}}}]}]}`},
		{FormatGemini, []byte(`[{"candidates":[{"content":{"parts":[{"text":"ans"}]}}]},{"candidates":[{"content":{"parts":[{"text":"wer"}]}}]}]`), `{"contents":[{"role":"model","parts":[{"text":"answer"}]}]}`},
	}
	for i, tt := range tests {
		t.Run(fmt.Sprintf("%s%d", tt.f, i), func(t *testing.T) {
			resp, err := ParseResponse(tt.f, tt.body, true)
			if err != nil {
				t.Fatal(err)
			}
			next := mustRequest(t, tt.f, tt.next)
			if len(resp.Elements) != len(next.Elements) {
				t.Fatal("count")
			}
			for j, e := range resp.Elements {
				if e.SemanticID != next.Elements[j].SemanticID {
					t.Fatalf("convergence\n%s\n%s", e.Raw, next.Elements[j].Raw)
				}
			}
		})
	}
	// A final completed event supersedes already received done items, once only.
	r, err := ParseResponse(FormatOpenAIResponses, sse(`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","summary":[]}}`, `{"type":"response.completed","response":{"id":"r","model":"o","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}}`), true)
	if err != nil || len(r.Elements) != 1 || !strings.Contains(r.Elements[0].Text, "answer") {
		t.Fatal(r, err)
	}
}

func TestChainAndTail(t *testing.T) {
	a := mustRequest(t, FormatOpenAIChat, `{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"a"}]}`)
	b := mustRequest(t, FormatOpenAIChat, `{"messages":[{"content":"a","role":"user"},{"content":"b","role":"assistant"},{"role":"user","content":"a"}]}`)
	chain := func(es []Element) []string {
		p := RootEntryID("actor", FormatOpenAIChat)
		out := []string{}
		for _, e := range es {
			p = EntryID(p, e.SemanticID)
			out = append(out, p)
		}
		return out
	}
	aa, bb := chain(a.Elements), chain(b.Elements)
	if !reflect.DeepEqual(aa, bb) {
		t.Fatal("resends differ")
	}
	if aa[0] == aa[2] {
		t.Fatal("position not hashed")
	}
	b.Elements[0] = element(FormatOpenAIChat, []byte(`{"role":"user","content":"edited"}`), "", false)
	bb = chain(b.Elements)
	for i := range aa {
		if aa[i] == bb[i] {
			t.Fatal("branch did not change")
		}
	}
	if TailStart(a.Elements) != 2 || TailStart(nil) != 0 || TailStart(a.Elements[:1]) != 0 || TailStart([]Element{{Role: "model"}}) != 1 {
		t.Fatal("tail")
	}
	if RootEntryID("actor", FormatClaude) == RootEntryID("actor", FormatGemini) || RootEntryID("other", FormatClaude) == RootEntryID("actor", FormatClaude) {
		t.Fatal("root isolation")
	}
}

func TestProjection(t *testing.T) {
	body := `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason words","signature":"SECRET_SIGNATURE"},{"type":"tool_use","id":"t","name":"read","input":{"path":"hello.go","command":"cat hello.go"}},{"type":"tool_result","tool_use_id":"t","content":"RESULT_WORDS"},{"type":"image","source":{"data":"SECRET_BASE64","media_type":"image/png"}},{"type":"image_url","image_url":{"url":"data:image/png;base64,SECRET_URI"}},{"type":"file","file_data":"SECRET_FILE"},{"type":"reasoning","encrypted_content":"SECRET_ENCRYPTED","summary":[{"text":"SUMMARY_WORDS"}]}]}]}`
	e := mustRequest(t, FormatClaude, body).Elements[0]
	for _, s := range []string{"reason words", "hello.go", "cat hello.go", "RESULT_WORDS", "SUMMARY_WORDS"} {
		if !strings.Contains(e.Text, s) {
			t.Fatalf("missing %s", s)
		}
	}
	if strings.Contains(e.Text, "SECRET") {
		t.Fatalf("leaked opaque content %s", e.Text)
	}
	if !reflect.DeepEqual(e.ToolCallIDs, []string{"t"}) || !reflect.DeepEqual(e.ToolNames, []string{"read"}) {
		t.Fatalf("tool metadata %+v", e)
	}
	huge := strings.Repeat("A", 65537)
	r := mustRequest(t, FormatOpenAIResponses, `{"input":[{"type":"reasoning","summary":[{"text":"`+huge+`"}]}]}`)
	if strings.Contains(r.Elements[0].Text, huge) {
		t.Fatal("base64 leaked")
	}
	gem := mustRequest(t, FormatGemini, `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"result":"tool result"}}},{"inline_data":{"data":"SECRET"}}]}]}`).Elements[0]
	if !strings.Contains(gem.Text, "tool result") || strings.Contains(gem.Text, "SECRET") || len(gem.BlockTypes) != 2 || len(gem.ToolNames) != 1 {
		t.Fatal(gem)
	}
}

func TestMalformed(t *testing.T) {
	for _, f := range []Format{FormatUnknown, FormatClaude, FormatGemini, FormatOpenAIChat, FormatOpenAIResponses} {
		for _, body := range []string{"", `null`, `[]`, `{}`, `{"error":{"message":"bad"}}`, `{"messages":42}`, `{"input":true}`, `{"messages":[null]}`, `{"messages":[1]}`, `{"messages":["bad"]}`} {
			if _, err := ParseRequest(f, []byte(body)); !errors.Is(err, ErrNotTranscript) {
				t.Fatalf("%s %s: %v", f, body, err)
			}
			for _, stream := range []bool{false, true} {
				if _, err := ParseResponse(f, []byte(body), stream); !errors.Is(err, ErrNotTranscript) {
					t.Fatalf("response %s %s: %v", f, body, err)
				}
			}
		}
	}
	r := mustRequest(t, FormatOpenAIChat, `{"messages":[{"role":"user","content":"hi"}]}`)
	for _, raw := range [][][]byte{nil, {[]byte(`invalid`)}, {[]byte(`{}`), []byte(`{}`)}} {
		if _, err := Rebuild(FormatOpenAIChat, r.Envelope, raw); err == nil {
			t.Fatal("expected rebuild error")
		}
	}
	for _, envelope := range []string{`bad`, `{}`, `{"messages":{"$cpa_transcript_v1":{"count":-1,"array":true}}}`, `{"messages":{"$cpa_transcript_v1":{"count":0,"array":false}}}`} {
		if _, err := Rebuild(FormatOpenAIChat, []byte(envelope), nil); err == nil {
			t.Fatal("expected malformed placeholder error")
		}
	}
}

func FuzzTranscript(f *testing.F) {
	for _, s := range []string{`{"messages":[{"role":"user","content":"hi"}]}`, `{"contents":[]}`, `{"input":"hi"}`, `data: {"type":"message_stop"}`, `null`, ""} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, format := range []Format{FormatUnknown, FormatOpenAIChat, FormatOpenAIResponses, FormatClaude, FormatGemini} {
			r, err := ParseRequest(format, b)
			if err == nil {
				rebuilt, err := Rebuild(format, r.Envelope, raws(r.Elements))
				if err != nil {
					t.Fatal(err)
				}
				sameJSON(t, b, rebuilt)
			}
			_, _ = ParseResponse(format, b, false)
			_, _ = ParseResponse(format, b, true)
			_, _ = Rebuild(format, b, nil)
			_ = DetectFormat("", b)
		}
	})
}

func BenchmarkParseRequest500KB(b *testing.B) {
	messages := make([]interface{}, 300)
	for i := range messages {
		messages[i] = map[string]interface{}{"role": "user", "content": strings.Repeat("synthetic words ", 112)}
	}
	body := jsonRaw(map[string]interface{}{"model": "test", "messages": messages})
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ParseRequest(FormatOpenAIChat, body); err != nil {
			b.Fatal(err)
		}
	}
}

// Exercise the ingester's actual chain construction across complete turns,
// including pre-existing context, newly returned output, and the next user turn.
func TestFullTurnChains(t *testing.T) {
	tests := []struct {
		f                            Format
		field, first, response, user string
	}{
		{FormatOpenAIChat, "messages", `{"role":"user","content":"first"}`, `{"choices":[{"message":{"role":"assistant","content":"answer"}}]}`, `{"role":"user","content":"second"}`},
		{FormatClaude, "messages", `{"role":"user","content":"first"}`, `{"type":"message","role":"assistant","content":[{"type":"text","text":"answer"}]}`, `{"role":"user","content":"second"}`},
		{FormatOpenAIResponses, "input", `{"role":"user","content":[{"type":"input_text","text":"first"}]}`, `{"output":[{"type":"message","id":"m","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[]}]}]}`, `{"role":"user","content":[{"type":"input_text","text":"second"}]}`},
		{FormatGemini, "contents", `{"role":"user","parts":[{"text":"first"}]}`, `{"candidates":[{"content":{"role":"model","parts":[{"text":"answer"}]}}]}`, `{"role":"user","parts":[{"text":"second"}]}`},
	}
	for _, tt := range tests {
		t.Run(string(tt.f), func(t *testing.T) {
			first := mustRequest(t, tt.f, `{"`+tt.field+`":[`+tt.first+`]}`)
			response, err := ParseResponse(tt.f, []byte(tt.response), false)
			if err != nil {
				t.Fatal(err)
			}
			history := append(append([]Element{}, first.Elements...), response.Elements...)
			all := append(raws(history), []byte(tt.user))
			next := mustRequest(t, tt.f, `{"`+tt.field+`":[`+string(bytes.Join(all, []byte(",")))+`]}`)
			parentA, parentB := RootEntryID("actor", tt.f), RootEntryID("actor", tt.f)
			for i, e := range history {
				parentA = EntryID(parentA, e.SemanticID)
				parentB = EntryID(parentB, next.Elements[i].SemanticID)
				if parentA != parentB {
					t.Fatal("resend created a new occurrence")
				}
			}
			if TailStart(next.Elements) != len(history) {
				t.Fatal("new-user tail is wrong")
			}
		})
	}
}

func TestSpecialWireShapes(t *testing.T) {
	// SSE data can span lines, including a payload larger than Scanner's 64 KiB.
	text := strings.Repeat("readable text ", 6000)
	stream := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\ndata: \"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"" + text + "\"}]}]}}\n\n")
	r, err := ParseResponse(FormatOpenAIResponses, stream, true)
	if err != nil || !strings.Contains(r.Elements[0].Text, text) {
		t.Fatal("multiline/large SSE", err)
	}
	// Preserve JSON number spelling and every non-transcript byte exactly.
	body := []byte("{\n  \"metadata\":{\"x\":1.000e+2}, \"messages\":[{ \"role\":\"user\", \"content\":\"hi\\nthere\"}], \"tools\":[]\n}")
	req, err := ParseRequest(FormatOpenAIChat, body)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := Rebuild(FormatOpenAIChat, req.Envelope, raws(req.Elements))
	if err != nil || !bytes.Equal(body, rebuilt) {
		t.Fatalf("lossy rebuild %s %v", rebuilt, err)
	}
	// Removing response metadata must not remove user tool input with the same key.
	a := mustRequest(t, FormatOpenAIResponses, `{"input":[{"type":"function_call","id":"outer","call_id":"call","arguments":"{}","input":{"id":"user-id","status":"user-status","signature":"user-signature"}}]}`).Elements[0]
	b := mustRequest(t, FormatOpenAIResponses, `{"input":[{"type":"function_call","call_id":"call","arguments":"{}","input":{"id":"edited","status":"user-status","signature":"user-signature"}}]}`).Elements[0]
	if a.SemanticID == b.SemanticID {
		t.Fatal("tool input id erased")
	}
	// Non-streaming Gemini CLI responses use the same response wrapper as SSE.
	r, err = ParseResponse(FormatGemini, []byte(`{"response":{"responseId":"g","modelVersion":"model","candidates":[{"content":{"parts":[{"text":"answer"}]}}]}}`), false)
	if err != nil || r.ResponseID != "g" || r.Model != "model" || r.Elements[0].Role != "model" {
		t.Fatal(r, err)
	}
}
