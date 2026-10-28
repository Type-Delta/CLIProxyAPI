package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/store"
)

// Synthetic text retains the real round-2 Claude Code/--continue and Codex wire
// structures, including multi-call messages, empty system messages and metadata.
func TestRound2RealClientCallAnchorsAndYardstick(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	const read = "toolu_01PUnJJHWmjvHvdBq1kETZAr"
	const arithmetic = "toolu_01FBEd5HXh1GiYcocewu6hwH"
	const cat = "toolu_01AB9yGa7AJ5iyi3C8m8m2JE"
	const codex = "call_d9e400391b5c4951a8380c74b02a6766"
	type obj = map[string]any
	encode := func(v any) []byte {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	tool := func(id, name, command string) obj {
		return obj{"type": "tool_use", "id": id, "name": name, "input": obj{"command": command}}
	}
	msg := func(role string, content any) obj { return obj{"role": role, "content": content} }
	result := func(id, text string) obj { return obj{"type": "tool_result", "tool_use_id": id, "content": text} }
	prompt := msg("user", []any{obj{"type": "text", "text": "read skill then calculate and reread", "cache_control": obj{"type": "ephemeral"}}})
	readCall := tool(read, "Read", "skill.md")
	readCall["caller"] = obj{"type": "direct"}
	response1 := msg("assistant", []any{readCall})
	deleteCaller := tool(read, "Read", "skill.md")
	history := []any{msg("user", "read skill then calculate and reread"), msg("system", []any{}), msg("assistant", []any{deleteCaller}), msg("user", []any{result(read, "AAA skill contents")})}
	history[1].(obj)["output_config"] = obj{"effort": "low"}
	response2 := msg("assistant", []any{tool(arithmetic, "Bash", "2+3"), tool(cat, "Bash", "cat skill.md")})
	completed := append(append([]any{}, history...), response2, msg("user", []any{result(arithmetic, "5"), result(cat, "AAA skill contents")}))
	continued := append(append([]any{}, completed...), msg("assistant", "5"), msg("system", []any{}), msg("user", "continue without reading"))
	requests := []*record.Request{}
	for n, tc := range []struct {
		messages []any
		reply    any
	}{{[]any{prompt}, response1}, {history, response2}, {completed, msg("assistant", "5")}, {continued, msg("assistant", "no additional read")}} {
		// A real client's earlier system/header drift must not duplicate a call.
		body := encode(obj{"system": []any{obj{"type": "text", "text": fmt.Sprintf("billing header version %d", n)}}, "messages": tc.messages})
		requests = append(requests, &record.Request{RequestID: fmt.Sprintf("claude-r2-%d", n), ActorKeyID: "actor", StartedAt: time.Now(), URL: "/v1/messages", Body: body, Response: encode(tc.reply)})
	}
	custom := obj{"type": "custom_tool_call", "call_id": codex, "id": "ctc_response_id", "name": "exec", "input": "cat skill.md", "status": "completed", "internal_chat_message_metadata_passthrough": obj{"create_time": 123.5, "turn_id": "turn1"}, "metadata": obj{"turn_id": "turn1"}}
	commentary := obj{"type": "message", "role": "assistant", "phase": "commentary", "content": []any{obj{"type": "output_text", "text": "reading skill", "annotations": []any{}}}, "internal_chat_message_metadata_passthrough": obj{"turn_id": "turn1"}}
	requests = append(requests, &record.Request{RequestID: "codex-r2-0", ActorKeyID: "actor", StartedAt: time.Now(), URL: "/v1/responses", Body: encode(obj{"instructions": "version 1", "input": []any{msg("user", "read once")}}), Response: encode(obj{"id": "response1", "output": []any{commentary, custom}})})
	customReplay := obj{"type": "custom_tool_call", "call_id": codex, "name": "exec", "input": "cat skill.md"}
	replay := []any{msg("user", "read once"), obj{"role": "assistant", "content": []any{obj{"type": "output_text", "text": "reading skill"}}}, customReplay, obj{"type": "custom_tool_call_output", "call_id": codex, "output": "AAA skill contents"}}
	requests = append(requests, &record.Request{RequestID: "codex-r2-1", ActorKeyID: "actor", StartedAt: time.Now(), URL: "/v1/responses", Body: encode(obj{"instructions": "version 2", "input": replay})}, &record.Request{RequestID: "codex-r2-2", ActorKeyID: "actor", StartedAt: time.Now(), URL: "/v1/responses", Body: encode(obj{"instructions": "version 3", "input": replay})})
	for _, r := range requests {
		if err = ing.Process(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	table := "`" + db.Database() + "`."
	for _, id := range []string{read, arithmetic, cat, codex} {
		for _, role := range []string{"assistant", "result"} {
			predicate := "role='assistant'"
			if role == "result" {
				predicate = "role!='assistant'"
			}
			var n uint64
			err = db.Conn().QueryRow(ctx, "SELECT uniqExact(entry_id) FROM "+table+"transcript_entries WHERE has(tool_call_ids,?) AND "+predicate, id).Scan(&n)
			if err != nil || n != 1 {
				t.Errorf("%s %s has %d entry IDs: %v", id, role, n, err)
			}
		}
	}
	var count uint64
	err = db.Conn().QueryRow(ctx, "SELECT uniqExact(e.entry_id) FROM "+table+"transcript_entries e JOIN "+table+"transcript_content c ON e.content_id=c.content_id WHERE e.first_attempt_index=0 AND e.occurred_at>=toStartOfMonth(now()) AND position(c.text,'AAA skill contents')>0").Scan(&count)
	if err != nil || count != 3 {
		t.Errorf("yardstick actual loads=%d want=3: %v", count, err)
	}
	var ids, contents []string
	if err = db.Conn().QueryRow(ctx, "SELECT entry_ids,content_ids FROM "+table+"request_contexts WHERE request_id='claude-r2-1'").Scan(&ids, &contents); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 5 || len(contents) != 5 || ids[2] != "" {
		t.Errorf("empty system was not excluded: ids=%v content IDs=%v", ids, contents)
	}
	for _, r := range requests {
		got, e := store.Request(ctx, db, r.RequestID)
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(got.Body, r.Body) {
			t.Fatalf("rebuild changed raw bytes for %s\n%s\n%s", r.RequestID, got.Body, r.Body)
		}
	}
}

func TestNonUniqueToolIDsRemainChainScoped(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	for _, id := range []string{"functions.Read:0", "call_0", "123", "call_00000000000000000000000000000000"} {
		for n := 0; n < 2; n++ {
			body := []byte(fmt.Sprintf(`{"input":[{"role":"user","content":"prefix %d"},{"type":"function_call","call_id":%q,"name":"read","arguments":"{}"}]}`, n, id))
			if err = ing.Process(ctx, &record.Request{RequestID: fmt.Sprintf("weak-%s-%d", id, n), ActorKeyID: "actor", StartedAt: time.Now(), URL: "/v1/responses", Body: body}); err != nil {
				t.Fatal(err)
			}
		}
		var n uint64
		err = db.Conn().QueryRow(ctx, "SELECT uniqExact(entry_id) FROM `"+db.Database()+"`.transcript_entries WHERE has(tool_call_ids,?)", id).Scan(&n)
		if err != nil || n != 2 {
			t.Errorf("nonunique %s merged calls: %d %v", id, n, err)
		}
	}
}

func TestAppendedUserTextRemainsSearchableAfterResultAnchor(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	const call = "toolu_01PUnJJHWmjvHvdBq1kETZAr"
	first := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + call + `","content":"original result"}]}]}`)
	appended := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + call + `","content":"original result"},{"type":"text","text":"please use skill BBB now"}]}]}`)
	for n, body := range [][]byte{first, appended, appended} {
		r := &record.Request{RequestID: fmt.Sprintf("appended-%d", n), ActorKeyID: "actor", StartedAt: time.Now(), URL: "/v1/messages", Body: body}
		if err = ing.Process(ctx, r); err != nil {
			t.Fatal(err)
		}
		got, e := store.Request(ctx, db, r.RequestID)
		if e != nil || !bytes.Equal(got.Body, body) {
			t.Fatalf("raw context reconstruction: %s %v", got.Body, e)
		}
	}
	table := "`" + db.Database() + "`."
	var stored, searchable uint64
	if err = db.Conn().QueryRow(ctx, "SELECT count() FROM "+table+"transcript_content WHERE position(text,'please use skill BBB now')>0").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == 0 {
		t.Fatal("appended text was not stored")
	}
	if err = db.Conn().QueryRow(ctx, "SELECT uniqExact(e.entry_id) FROM "+table+"transcript_entries e JOIN "+table+"transcript_content c ON e.content_id=c.content_id WHERE e.first_attempt_index=0 AND position(c.text,'please use skill BBB now')>0").Scan(&searchable); err != nil {
		t.Fatal(err)
	}
	if searchable != 1 {
		t.Fatalf("stored appended text lost from occurrence search: matches=%d want=1", searchable)
	}
}
