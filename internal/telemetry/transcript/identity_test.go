package transcript

import "testing"

func TestProviderCallIDGuardAndAnchorScopes(t *testing.T) {
	const strong = "call_d9e400391b5c4951a8380c74b02a6766"
	for _, id := range []string{strong, "toolu_01PUnJJHWmjvHvdBq1kETZAr", "f47ac10b-58cc-4372-a567-0e02b2c3d479"} {
		if !uniqueProviderCallID(id) {
			t.Errorf("rejected unique-looking id %s", id)
		}
	}
	for _, id := range []string{"call_0", "functions.Read:0", "123", "call_00000000000000000000000000000000", "call_0000000000000000000000001234567a", "call_abcdefghijklmnopqrstuvwx"} {
		if uniqueProviderCallID(id) {
			t.Errorf("accepted weak id %s", id)
		}
	}
	e := element(FormatOpenAIResponses, []byte(`{"type":"function_call","call_id":"`+strong+`","name":"read","arguments":"{}"}`), "", false)
	id := OccurrenceID("actor", FormatOpenAIResponses, "parent1", e)
	if got := OccurrenceID("actor", FormatOpenAIResponses, "parent2", e); got != id {
		t.Fatal("parent drift changed call anchor")
	}
	for _, got := range []string{OccurrenceID("other-actor", FormatOpenAIResponses, "parent1", e), OccurrenceID("actor", FormatOpenAIChat, "parent1", e)} {
		if got == id {
			t.Fatal("anchor leaked across actor/format")
		}
	}
	result := element(FormatOpenAIResponses, []byte(`{"type":"function_call_output","call_id":"`+strong+`","output":"answer"}`), "", false)
	if OccurrenceID("actor", FormatOpenAIResponses, "parent1", result) == id {
		t.Fatal("use and result merged")
	}
	user := e
	user.Role = "user"
	if OccurrenceID("actor", FormatOpenAIResponses, "parent1", user) == id {
		t.Fatal("roles merged")
	}
	text := element(FormatOpenAIResponses, []byte(`{"role":"assistant","content":"after anchored call"}`), "", false)
	// Different prefixes now produce the same next text occurrence after a call.
	if OccurrenceID("actor", FormatOpenAIResponses, OccurrenceID("actor", FormatOpenAIResponses, "p1", e), text) != OccurrenceID("actor", FormatOpenAIResponses, OccurrenceID("actor", FormatOpenAIResponses, "p2", e), text) {
		t.Fatal("text chain did not resynchronize")
	}
}
func TestCallAnchorsUseProtocolIDsOnlyAndSort(t *testing.T) {
	const a = "toolu_01PUnJJHWmjvHvdBq1kETZAr"
	const b = "toolu_01FBEd5HXh1GiYcocewu6hwH"
	one := element(FormatClaude, []byte(`{"role":"assistant","content":[{"type":"tool_use","id":"`+a+`","name":"read","input":{"call_id":"user data"}},{"type":"tool_use","id":"`+b+`","name":"read","input":{}}]}`), "", false)
	two := element(FormatClaude, []byte(`{"role":"assistant","content":[{"type":"tool_use","id":"`+b+`","name":"read","input":{}},{"type":"tool_use","id":"`+a+`","name":"read","input":{}}]}`), "", false)
	if OccurrenceID("actor", FormatClaude, "p1", one) != OccurrenceID("actor", FormatClaude, "p2", two) {
		t.Fatal("call order or user payload IDs affected anchor")
	}
	mixed := element(FormatClaude, []byte(`{"role":"assistant","content":[{"type":"tool_use","id":"`+a+`","name":"read","input":{}},{"type":"tool_use","id":"call_0","name":"read","input":{}}]}`), "", false)
	if OccurrenceID("actor", FormatClaude, "p1", mixed) == OccurrenceID("actor", FormatClaude, "p2", mixed) {
		t.Fatal("one strong ID hid a weak ID")
	}
}
func TestNoSemanticContentHasNoOccurrence(t *testing.T) {
	for _, raw := range []string{`{"role":"system","content":[],"output_config":{"effort":"low"}}`, `{"role":"system","content":""}`, `{"type":"reasoning","encrypted_content":"opaque","summary":[]}`} {
		e := element(FormatClaude, []byte(raw), "", false)
		if id := OccurrenceID("actor", FormatClaude, "parent", e); id != "" {
			t.Errorf("empty element got entry %s: %s", id, raw)
		}
		if e.ContentID == "" || len(e.Raw) == 0 {
			t.Fatal("empty semantic element lost raw content")
		}
	}
}

func TestUnknownTypeOnlyContentIsNotEmpty(t *testing.T) {
	for _, raw := range []string{`{"type":"future_marker"}`, `{"role":"assistant","content":[{"type":"future_marker"}]}`, `{"type":"computer_call","action":{"type":"wait"}}`} {
		e := element(FormatOpenAIResponses, []byte(raw), "", false)
		if OccurrenceID("actor", FormatOpenAIResponses, "parent", e) == "" {
			t.Errorf("unknown typed semantic content was discarded: %s", raw)
		}
	}
}

func TestSynthesizedCallIDsStayChainScoped(t *testing.T) {
	// The translator emits call_ plus 24 lower-case hexadecimal characters from
	// a deterministic hash of position/name/payload, with no session identity.
	const id = "call_6c2a1f3e9b7d4c218a5e1f2e"
	if uniqueProviderCallID(id) {
		t.Error("deterministic CPA ID qualified as globally unique")
	}
	e := element(FormatOpenAIResponses, []byte(`{"type":"function_call","call_id":"`+id+`","name":"read","arguments":"{}"}`), "", false)
	if OccurrenceID("actor", FormatOpenAIResponses, "session-prefix-a", e) == OccurrenceID("actor", FormatOpenAIResponses, "session-prefix-b", e) {
		t.Fatal("synthesized ID overrode distinct session histories")
	}
	// Similar provider IDs of other lengths still use the ordinary entropy guard.
	if !uniqueProviderCallID("call_d9e400391b5c4951a8380c74b02a6766") {
		t.Fatal("real Codex provider ID no longer anchors")
	}
}
func TestCallAnchorDesignedMergeAndGroupingLimits(t *testing.T) {
	const a = "toolu_01PUnJJHWmjvHvdBq1kETZAr"
	const b = "toolu_01AB9yGa7AJ5iyi3C8m8m2JE"
	occurrence := func(raw string) string {
		return OccurrenceID("actor", FormatClaude, "parent", element(FormatClaude, []byte(raw), "", false))
	}
	first := occurrence(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + a + `","content":"first result"}]}`)
	changed := occurrence(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + a + `","content":"changed result"}]}`)
	if first != changed {
		t.Fatal("provider result ID is no longer authoritative")
	}
	grouped := occurrence(`{"role":"assistant","content":[{"type":"tool_use","id":"` + a + `","name":"read","input":{}},{"type":"tool_use","id":"` + b + `","name":"read","input":{}}]}`)
	single := occurrence(`{"role":"assistant","content":[{"type":"tool_use","id":"` + a + `","name":"read","input":{}}]}`)
	if grouped == single {
		t.Fatal("different call-ID sets unexpectedly converged")
	}
}
