package transcript

import (
	"encoding/json"
	"strings"
	"testing"
)

// Synthetic payloads retain the wire structures observed in Claude Code and Codex live QA.
func TestRealClientSemanticConvergence(t *testing.T) {
	for _, tc := range []struct {
		name             string
		f                Format
		response, resend string
	}{
		{"claude-code", FormatClaude, `{"role":"assistant","content":[{"type":"tool_use","id":"tool_1","name":"Read","input":{"file_path":"/skills/AAA.md","metadata":{"data":"keep","caller":"user input"}},"caller":{"type":"direct"}}]}`, `{"role":"assistant","content":[{"type":"tool_use","id":"tool_1","name":"Read","input":{"file_path":"/skills/AAA.md","metadata":{"data":"keep","caller":"user input"}}}]}`},
		{"codex-custom", FormatOpenAIResponses, `{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"exec","input":"cat /skills/AAA.md","status":"completed","namespace":"functions","metadata":{"request_id":"r1"},"request_id":"r1"}`, `{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"exec","input":"cat /skills/AAA.md","status":"completed","namespace":"functions"}`},
		{"codex-function", FormatOpenAIResponses, `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"data\":\"keep\"}","status":"completed","provider_added":{"anything":true}}`, `{"type":"function_call","call_id":"call_1","name":"read","arguments":"{\"data\":\"keep\"}"}`},
		{"codex-text", FormatOpenAIResponses, `{"type":"message","role":"assistant","id":"msg_1","phase":"final_answer","content":[{"type":"output_text","text":"done","annotations":[{"type":"citation","url":"https://example.test"}],"new_metadata":"value"}]}`, `{"role":"assistant","content":[{"type":"output_text","text":"done"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := element(tc.f, []byte(tc.response), "", false), element(tc.f, []byte(tc.resend), "", false)
			if a.SemanticID != b.SemanticID {
				t.Fatalf("response/resend diverged: %s != %s", a.SemanticID, b.SemanticID)
			}
			if a.ContentID == b.ContentID {
				t.Fatal("raw bytes must remain distinct")
			}
			changed := strings.Replace(tc.resend, "keep", "changed", 1)
			if changed != tc.resend && element(tc.f, []byte(changed), "", false).SemanticID == b.SemanticID {
				t.Fatal("discarded semantic tool input")
			}
		})
	}
}
func TestSearchableDataField(t *testing.T) {
	e := element(FormatClaude, []byte(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":{"data":"skill AAA content","nested":{"data":"ordinary text"},"binary":{"data":"data:image/png;base64,AAAA"}}}]}`), "", false)
	if !strings.Contains(e.Text, "skill AAA content") || !strings.Contains(e.Text, "ordinary text") || strings.Contains(e.Text, "base64") {
		t.Fatal(e.Text)
	}
}

func TestWhitelistKeepsRolesAndOpaqueToolData(t *testing.T) {
	system := element(FormatOpenAIResponses, []byte(`"same text"`), "system", true)
	user := element(FormatOpenAIResponses, []byte(`"same text"`), "user", false)
	if system.SemanticID == user.SemanticID {
		t.Error("inferred roles merged")
	}
	for _, tc := range []struct {
		f    Format
		a, b string
	}{
		{FormatOpenAIChat, `{"role":"assistant","content":"answer","provider_metadata":{"id":"response"}}`, `{"role":"assistant","content":"answer"}`},
		{FormatGemini, `{"role":"model","parts":[{"functionCall":{"id":"call","name":"read","args":{"cache_control":"user data","status":"user data"},"provider_metadata":true},"thoughtSignature":"signature"}]}`, `{"role":"model","parts":[{"functionCall":{"id":"call","name":"read","args":{"cache_control":"user data","status":"user data"}}}]}`},
	} {
		a, b := element(tc.f, []byte(tc.a), "", false), element(tc.f, []byte(tc.b), "", false)
		if a.SemanticID != b.SemanticID {
			t.Fatalf("metadata affected %s semantics", tc.f)
		}
		changed := strings.Replace(tc.b, "user data", "different input", 1)
		if changed != tc.b && element(tc.f, []byte(changed), "", false).SemanticID == b.SemanticID {
			t.Fatal("tool input lost")
		}
	}
}

func TestConservativeCanonicalizationDistinctPayloads(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    Format
		a, b string
	}{
		{"web_search_call", FormatOpenAIResponses, `{"type":"web_search_call","id":"ws1","status":"completed","action":{"type":"search","query":"first query"}}`, `{"type":"web_search_call","id":"ws1","status":"completed","action":{"type":"search","query":"second query"}}`},
		{"local_shell_call", FormatOpenAIResponses, `{"type":"local_shell_call","call_id":"shell1","action":{"type":"exec","command":["cat","first.txt"]}}`, `{"type":"local_shell_call","call_id":"shell1","action":{"type":"exec","command":["cat","second.txt"]}}`},
		{"computer_call", FormatOpenAIResponses, `{"type":"computer_call","call_id":"computer1","action":{"type":"click","x":10,"y":20}}`, `{"type":"computer_call","call_id":"computer1","action":{"type":"click","x":30,"y":20}}`},
		{"item_reference", FormatOpenAIResponses, `{"type":"item_reference","id":"reference1"}`, `{"type":"item_reference","id":"reference2"}`},
		{"mcp_tool_use", FormatClaude, `{"type":"mcp_tool_use","id":"tool1","name":"read","server_name":"server1","input":{"path":"first.txt"}}`, `{"type":"mcp_tool_use","id":"tool1","name":"read","server_name":"server1","input":{"path":"second.txt"}}`},
		{"document", FormatClaude, `{"type":"document","source":{"type":"content","content":[{"type":"text","text":"first document"}]}}`, `{"type":"document","source":{"type":"content","content":[{"type":"text","text":"second document"}]}}`},
		{"tool_result", FormatClaude, `{"type":"tool_result","tool_use_id":"tool1","content":"first result"}`, `{"type":"tool_result","tool_use_id":"tool1","content":"second result"}`},
		{"known-text-new-payload", FormatOpenAIResponses, `{"type":"output_text","text":"same","translation":"first meaning"}`, `{"type":"output_text","text":"same","translation":"second meaning"}`},
		{"known-message-new-payload", FormatClaude, `{"role":"assistant","content":[],"new_content":{"answer":"first"}}`, `{"role":"assistant","content":[],"new_content":{"answer":"second"}}`},
		{"known-gemini-new-payload", FormatGemini, `{"role":"model","parts":[{"text":"same","new_part":{"data":"first"}}]}`, `{"role":"model","parts":[{"text":"same","new_part":{"data":"second"}}]}`},
		{"known-gemini-function-new-payload", FormatGemini, `{"role":"model","parts":[{"functionCall":{"name":"read","args":{},"new_input":"first"}}]}`, `{"role":"model","parts":[{"functionCall":{"name":"read","args":{},"new_input":"second"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := element(tc.f, []byte(tc.a), "", false), element(tc.f, []byte(tc.b), "", false)
			if a.SemanticID == b.SemanticID {
				t.Error("distinct payloads collapsed")
			}
			root := RootEntryID("same-actor", tc.f)
			if EntryID(root, a.SemanticID) == EntryID(root, b.SemanticID) {
				t.Error("different conversations at the same chain position merged")
			}
			for _, raw := range []string{tc.a, tc.b} {
				var reordered interface{}
				if err := json.Unmarshal([]byte(raw), &reordered); err != nil {
					t.Fatal(err)
				}
				resend, err := json.Marshal(reordered)
				if err != nil {
					t.Fatal(err)
				}
				if element(tc.f, []byte(resend), "", false).SemanticID != element(tc.f, []byte(raw), "", false).SemanticID {
					t.Fatal("identical resend changed semantic identity")
				}
			}
		})
	}
}

func TestUnknownTypeFallbackDropsOnlyVolatileMetadata(t *testing.T) {
	a := `{"type":"future_tool_call","call_id":"c1","action":{"command":"read","input":{"id":"user id","status":"user status","signature":"user signature","cache_control":"user cache"}},"status":"completed","id":"transport-id","cache_control":{"type":"ephemeral"},"encrypted_content":"opaque","signature":"old","caller":{"type":"direct"},"metadata":{"request_id":"r1"},"request_id":"r1"}`
	b := `{"type":"future_tool_call","call_id":"c1","action":{"input":{"cache_control":"user cache","signature":"user signature","status":"user status","id":"user id"},"command":"read"}}`
	first, resend := element(FormatOpenAIResponses, []byte(a), "", false), element(FormatOpenAIResponses, []byte(b), "", false)
	if first.SemanticID != resend.SemanticID {
		t.Fatal("known volatile metadata survived fallback")
	}
	for _, key := range []string{"user id", "user status", "user signature", "user cache", "read"} {
		changed := strings.Replace(b, key, "different", 1)
		if element(FormatOpenAIResponses, []byte(changed), "", false).SemanticID == resend.SemanticID {
			t.Fatalf("fallback discarded semantic payload %q", key)
		}
	}
}

func TestClaudeStringAndTextBlockConverge(t *testing.T) {
	a := mustRequest(t, FormatClaude, `{"messages":[{"role":"user","content":"same prompt"}]}`)
	b := mustRequest(t, FormatClaude, `{"messages":[{"role":"user","content":[{"type":"text","text":"same prompt","cache_control":{"type":"ephemeral"}}]}]}`)
	if a.Elements[0].SemanticID != b.Elements[0].SemanticID {
		t.Fatal("Claude string/text-block forms diverge")
	}
}
func TestCodexInternalPassthroughIsVolatile(t *testing.T) {
	a := element(FormatOpenAIResponses, []byte(`{"type":"custom_tool_call","call_id":"call_d9e400391b5c4951a8380c74b02a6766","name":"exec","input":"read skill","internal_chat_message_metadata_passthrough":{"create_time":123.5,"turn_id":"turn-old"}}`), "", false)
	b := element(FormatOpenAIResponses, []byte(`{"type":"custom_tool_call","call_id":"call_d9e400391b5c4951a8380c74b02a6766","name":"exec","input":"read skill"}`), "", false)
	if a.SemanticID != b.SemanticID {
		t.Fatal("Codex passthrough metadata is semantic")
	}
}
func TestTranscriptRootUsesV2(t *testing.T) {
	if got, want := RootEntryID("actor", FormatClaude), digest([]byte("cpa-transcript-v2\x00actor\x00claude")); got != want {
		t.Fatalf("root=%s want v2=%s", got, want)
	}
}
