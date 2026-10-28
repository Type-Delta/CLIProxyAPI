package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/outbox"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/store"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/transcript"
)

func TestDrainConversation(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ing := New(s, dir, 0, 30)
	now := time.Now().UTC()
	records := []*record.Request{
		{RequestID: "one", StartedAt: now, URL: "/v1/messages", Method: "POST", ActorKeyID: "actor", SessionID: "session", Status: 403, Body: []byte(`{"model":"m","messages":[{"role":"user","content":"read skill AAA"}]}`), Response: []byte(`{"role":"assistant","content":[{"type":"text","text":"done"}]}`)},
		{RequestID: "two", StartedAt: now.Add(time.Second), URL: "/v1/messages", Method: "POST", ActorKeyID: "actor", SessionID: "session", Body: []byte(`{"model":"m","messages":[{"role":"user","content":"read skill AAA"},{"role":"assistant","content":[{"type":"text","text":"done"}]},{"role":"user","content":"read skill AAA"}]}`)},
	}
	for _, r := range records {
		if err = outbox.Write(dir, r); err != nil {
			t.Fatal(err)
		}
	}
	if err = ing.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	files, err := outbox.List(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("outbox %v %v", files, err)
	}
	var n uint64
	if err = db.Conn().QueryRow(ctx, "SELECT uniqExact(e.entry_id) FROM `"+db.Database()+"`.transcript_entries e INNER JOIN `"+db.Database()+"`.transcript_content c ON e.content_id = c.content_id WHERE c.text LIKE '%skill AAA%'").Scan(&n); err != nil || n != 2 {
		t.Fatalf("occurrences %d: %v", n, err)
	}
	if err = db.Conn().QueryRow(ctx, "SELECT count() FROM `"+db.Database()+"`.requests WHERE session_id = 'session' AND status = 403").Scan(&n); err != nil || n != 1 {
		t.Fatalf("403s %d: %v", n, err)
	}
}

func TestLowDiskKeepsOutbox(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = outbox.Write(dir, &record.Request{RequestID: "kept", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	ing := New(s, dir, ^uint64(0), 30)
	if err = ing.Drain(ctx); err == nil {
		t.Fatal("expected low disk error")
	}
	files, err := outbox.List(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("pending files %v: %v", files, err)
	}
	ing.minFree = 0
	if err = ing.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	files, err = outbox.List(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("pending files after resume %v: %v", files, err)
	}
}

func TestContextRetainsExactResentContent(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	now := time.Now().UTC()
	first := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}]}`)
	second := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hello"}],"cache_control":{"type":"ephemeral"}}]}`)
	for n, body := range [][]byte{first, second} {
		r := &record.Request{RequestID: fmt.Sprint(n), StartedAt: now.Add(time.Duration(n) * time.Second), URL: "/v1/messages", ActorKeyID: "a", Body: body}
		if err = ing.IngestRecords(ctx, []*record.Request{r}); err != nil {
			t.Fatal(err)
		}
	}
	table := "`" + db.Database() + "`."
	var entries, contents uint64
	if err = db.Conn().QueryRow(ctx, "SELECT uniqExact(entry_id) FROM "+table+"transcript_entries").Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if err = db.Conn().QueryRow(ctx, "SELECT uniqExact(content_id) FROM "+table+"transcript_content WHERE kind='element'").Scan(&contents); err != nil {
		t.Fatal(err)
	}
	if entries != 1 || contents != 2 {
		t.Fatalf("entries %d contents %d", entries, contents)
	}
	var ids []string
	if err = db.Conn().QueryRow(ctx, "SELECT content_ids FROM "+table+"request_contexts WHERE request_id='1'").Scan(&ids); err != nil {
		t.Fatal(err)
	}
	var raw, envelope string
	if err = db.Conn().QueryRow(ctx, "SELECT raw FROM "+table+"transcript_content WHERE content_id=?", ids[0]).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err = db.Conn().QueryRow(ctx, "SELECT c.raw FROM "+table+"requests r JOIN "+table+"transcript_content c ON c.content_id=r.envelope_content_id WHERE r.request_id='1'").Scan(&envelope); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := transcript.Rebuild(transcript.FormatClaude, []byte(envelope), [][]byte{[]byte(raw)})
	if err != nil || !bytes.Equal(rebuilt, second) {
		t.Fatalf("rebuilt %s: %v", rebuilt, err)
	}
	if err = db.Conn().Exec(ctx, "ALTER TABLE "+table+"transcript_content UPDATE first_observed_at = now()-INTERVAL 60 DAY WHERE content_id=? SETTINGS mutations_sync=2", ids[0]); err != nil {
		t.Fatal(err)
	}
	if err = s.GC(ctx, 30); err != nil {
		t.Fatal(err)
	}
	got, err := store.Request(ctx, db, "1")
	if err != nil || !bytes.Equal(got.Body, second) {
		t.Fatalf("GC lost exact context content: %v %v", got, err)
	}

}

func TestContinuationBothViewsAndMissing(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	now := time.Now().UTC()
	body := []byte(`{"input":[{"role":"user","content":"hi"}]}`)
	resp := []byte(`{"id":"resp1","object":"response","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`)
	r := &record.Request{RequestID: "first", StartedAt: now, URL: "/v1/responses", ActorKeyID: "actor", Body: body, Response: resp, Attempts: []record.Attempt{{Index: 1, URL: "/v1/responses", Body: body, Response: resp}}}
	if err = ing.IngestRecords(ctx, []*record.Request{r}); err != nil {
		t.Fatal(err)
	}
	next := []byte(`{"previous_response_id":"resp1","input":[{"role":"user","content":"next"}]}`)
	r = &record.Request{RequestID: "second", StartedAt: now.Add(time.Second), URL: "/v1/responses", ActorKeyID: "actor", Body: next, Attempts: []record.Attempt{{Index: 1, URL: "/v1/responses", Body: next}}}
	if err = ing.IngestRecords(ctx, []*record.Request{r}); err != nil {
		t.Fatal(err)
	}
	table := "`" + db.Database() + "`."
	rows, err := db.Conn().Query(ctx, "SELECT e.depth FROM "+table+"request_contexts c ARRAY JOIN c.entry_ids AS id INNER JOIN "+table+"transcript_entries e ON e.entry_id=id WHERE c.request_id='second'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var depth uint32
		if err = rows.Scan(&depth); err != nil {
			t.Fatal(err)
		}
		if depth != 2 {
			t.Fatalf("depth %d", depth)
		}
		n++
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("views %d", n)
	}
	r = &record.Request{RequestID: "missing", StartedAt: now, URL: "/v1/responses", Body: []byte(`{"previous_response_id":"absent","input":[{"role":"user","content":"x"}]}`)}
	if err = ing.IngestRecords(ctx, []*record.Request{r}); err != nil {
		t.Fatalf("missing continuation: %v", err)
	}
}

func TestReadRebuildsRequest(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	r := &record.Request{RequestID: "long-12345678", StartedAt: time.Now().UTC(), URL: "/v1/messages", Body: body, Status: 403, Detail: "error", Attempts: []record.Attempt{{Index: 1, URL: "/v1/messages", Body: body}}}
	if err = ing.IngestRecords(ctx, []*record.Request{r}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Request(ctx, db, "12345678")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Body, body) || len(got.Attempts) != 1 || !bytes.Equal(got.Attempts[0].Body, body) {
		t.Fatalf("bad rebuild: %+v", got)
	}
}

func TestGCKeepsReferencedContentAndResponse(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	now := time.Now().UTC()
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	response := []byte(`{"role":"assistant","content":[{"type":"text","text":"answer"}]}`)
	r := &record.Request{RequestID: "live", StartedAt: now, URL: "/v1/messages", Body: body, Response: response}
	if err = ing.IngestRecords(ctx, []*record.Request{r}); err != nil {
		t.Fatal(err)
	}
	table := "`" + db.Database() + "`."
	for _, name := range []string{"transcript_entries", "transcript_content"} {
		if err = db.Conn().Exec(ctx, "ALTER TABLE "+table+name+" UPDATE first_observed_at = now() - INTERVAL 60 DAY WHERE 1 SETTINGS mutations_sync=2"); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Conn().Exec(ctx, "INSERT INTO "+table+"transcript_content (content_id,kind,raw,text,bytes,first_observed_at) VALUES ('orphan','element','x','x',1,now()-INTERVAL 60 DAY)"); err != nil {
		t.Fatal(err)
	}
	if err = s.GC(ctx, 30); err != nil {
		t.Fatal(err)
	}
	var n uint64
	if err = db.Conn().QueryRow(ctx, "SELECT count() FROM "+table+"transcript_entries WHERE origin='response'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("response entries=%d err=%v", n, err)
	}
	if err = db.Conn().QueryRow(ctx, "SELECT count() FROM "+table+"transcript_content WHERE content_id='orphan'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("orphan=%d err=%v", n, err)
	}
	got, err := store.Request(ctx, db, "live")
	if err != nil || !bytes.Equal(got.Body, body) {
		t.Fatalf("rebuild %v %v", got, err)
	}
}

func TestSyntheticStorage200Turns(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	now := time.Now().UTC()
	history := []map[string]any{}
	var rawBytes int
	for n := 0; n < 200; n++ {
		history = append(history, map[string]any{"role": "user", "content": fmt.Sprintf("Read skill AAA for task %03d: %s", n, strings.Repeat("project source and implementation details ", 10))})
		body, _ := json.Marshal(map[string]any{"model": "claude", "messages": history})
		response, _ := json.Marshal(map[string]any{"role": "assistant", "content": []map[string]string{{"type": "text", "text": fmt.Sprintf("completed task %03d", n)}}})
		r := &record.Request{RequestID: fmt.Sprintf("req-%03d", n), StartedAt: now.Add(time.Duration(n) * time.Second), URL: "/v1/messages", Body: body, Response: response}
		rawBytes += len(body)
		if err = ing.IngestRecords(ctx, []*record.Request{r}); err != nil {
			t.Fatal(err)
		}
		history = append(history, map[string]any{"role": "assistant", "content": []map[string]string{{"type": "text", "text": fmt.Sprintf("completed task %03d", n)}}})
	}
	for _, name := range []string{"requests", "attempts", "request_contexts", "transcript_entries", "transcript_content"} {
		if err = db.Conn().Exec(ctx, "OPTIMIZE TABLE `"+db.Database()+"`."+name+" FINAL"); err != nil {
			t.Fatal(err)
		}
	}
	var disk uint64
	if err = db.Conn().QueryRow(ctx, "SELECT sum(bytes_on_disk) FROM system.parts WHERE active AND database=? AND table IN ('requests','attempts','request_contexts','transcript_entries','transcript_content')", db.Database()).Scan(&disk); err != nil {
		t.Fatal(err)
	}
	t.Logf("200 turns: raw request bodies=%d bytes; five tables on disk=%d bytes", rawBytes, disk)
}

func TestMidConversationAndWebsocketAttempt(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	now := time.Now().UTC()
	body := []byte(`{"messages":[{"role":"user","content":"old question"},{"role":"assistant","content":"old answer"},{"role":"user","content":"new question"}]}`)
	request := `{"type":"response.create","input":[{"role":"user","content":"websocket message"}]}`
	frame := `{"type":"response.completed","response":{"id":"ws-id","object":"response","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"websocket answer"}]}]}}`
	ev1, _ := json.Marshal(map[string]string{"event": "request", "payload": request})
	ev2, _ := json.Marshal(map[string]string{"event": "response", "payload": frame})
	timeline := append(append(ev1, '\n'), ev2...)
	r := &record.Request{RequestID: "mid", StartedAt: now, URL: "/v1/messages", Body: body, Attempts: []record.Attempt{{Index: 1, URL: "/v1/responses", Transport: "websocket", WebsocketTimeline: timeline}}}
	if err = ing.IngestRecords(ctx, []*record.Request{r}); err != nil {
		t.Fatal(err)
	}
	table := "`" + db.Database() + "`."
	var n uint64
	if err = db.Conn().QueryRow(ctx, "SELECT count() FROM "+table+"transcript_entries WHERE first_attempt_index=0 AND occurred_at IS NULL").Scan(&n); err != nil || n != 2 {
		t.Fatalf("history=%d: %v", n, err)
	}
	if err = db.Conn().QueryRow(ctx, "SELECT count() FROM "+table+"transcript_entries e JOIN "+table+"transcript_content c ON e.content_id=c.content_id WHERE e.first_attempt_index=1 AND c.text LIKE '%websocket answer%'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("websocket response=%d: %v", n, err)
	}
}

func TestOutboxReplayHasNoVisibleDuplicates(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ing := New(s, dir, 0, 30)
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	r := &record.Request{RequestID: "replay", StartedAt: time.Now().UTC(), URL: "/v1/messages", Body: body, Attempts: []record.Attempt{{Index: 1, URL: "/v1/messages", Body: body}}}
	table := "`" + db.Database() + "`."
	names := []string{"requests", "attempts", "request_contexts", "transcript_entries", "transcript_content"}
	var before []uint64
	for iteration := 0; iteration < 2; iteration++ {
		if err = outbox.Write(dir, r); err != nil {
			t.Fatal(err)
		}
		if err = ing.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		var counts []uint64
		for _, name := range names {
			var n uint64
			if err = db.Conn().QueryRow(ctx, "SELECT count() FROM "+table+name).Scan(&n); err != nil {
				t.Fatal(err)
			}
			counts = append(counts, n)
		}
		if iteration == 0 {
			before = counts
		} else if !reflect.DeepEqual(before, counts) {
			t.Fatalf("replay changed row counts: %v -> %v", before, counts)
		}
	}
}

func TestRepeatedToolReadCountsOccurrences(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	at := time.Now().UTC()
	first := `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call1","name":"Read","input":{"file_path":"/skills/AAA/SKILL.md"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":"skill AAA contents"}]}]}`
	second := `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call1","name":"Read","input":{"file_path":"/skills/AAA/SKILL.md"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":"skill AAA contents"}]},{"role":"assistant","content":[{"type":"tool_use","id":"call2","name":"Read","input":{"file_path":"/skills/AAA/SKILL.md"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":"skill AAA contents"}]}]}`
	for n, b := range []string{first, second, second} {
		r := &record.Request{RequestID: fmt.Sprintf("tool-%d", n), StartedAt: at.Add(time.Duration(n) * time.Second), URL: "/v1/messages", Body: []byte(b), SessionID: "xb12345"}
		if n == 0 {
			r.Status = 403
			r.Attempts = []record.Attempt{{Index: 1, Status: 403, URL: "/v1/messages", Body: []byte(b)}}
		}
		if err = ing.IngestRecords(ctx, []*record.Request{r}); err != nil {
			t.Fatal(err)
		}
	}
	var n uint64
	table := "`" + db.Database() + "`."
	err = db.Conn().QueryRow(ctx, "SELECT uniqExact(e.entry_id) FROM "+table+"transcript_entries e JOIN "+table+"transcript_content c ON e.content_id=c.content_id WHERE c.text LIKE '%skill AAA contents%'").Scan(&n)
	if err != nil || n != 2 {
		t.Fatalf("tool occurrences=%d err=%v", n, err)
	}
	err = db.Conn().QueryRow(ctx, "SELECT count() FROM "+table+"requests AS r LEFT JOIN "+table+"attempts AS a USING (request_id) WHERE r.session_id = ? AND (r.status=403 OR a.status=403)", "xb12345").Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("session 403 requests=%d err=%v", n, err)
	}
	err = db.Conn().QueryRow(ctx, "SELECT uniqExact(e.entry_id) FROM "+table+"transcript_entries e INNER JOIN "+table+"transcript_content c ON e.content_id=c.content_id WHERE c.text LIKE '%skill AAA contents%' AND e.occurred_at >= toStartOfMonth(now()) AND e.occurred_at < addMonths(toStartOfMonth(now()),1)").Scan(&n)
	if err != nil || n != 2 {
		t.Fatalf("monthly reads=%d err=%v", n, err)
	}

}

func TestConcurrentGCAndIngestion(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	start := make(chan struct{})
	done := make(chan error, 2)
	go func() {
		<-start
		done <- ing.IngestRecords(ctx, []*record.Request{{RequestID: "gc-race", StartedAt: time.Now().UTC(), URL: "/v1/messages", Body: []byte(`{"messages":[{"role":"user","content":"retained"}]}`)}})
	}()
	go func() { <-start; done <- s.GC(ctx, 30) }()
	close(start)
	for n := 0; n < 2; n++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	r, err := store.Request(ctx, db, "gc-race")
	if err != nil || !strings.Contains(string(r.Body), "retained") {
		t.Fatalf("record %v: %v", r, err)
	}
}

func TestContinuationAfterEmptyInput(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	now := time.Now().UTC()
	reply := func(id, text string) []byte {
		return []byte(fmt.Sprintf(`{"id":%q,"object":"response","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}]}`, id, text))
	}
	rs := []*record.Request{{RequestID: "a", StartedAt: now, URL: "/v1/responses", Body: []byte(`{"input":[{"role":"user","content":"hi"}]}`), Response: reply("r1", "one")}, {RequestID: "b", StartedAt: now.Add(time.Second), URL: "/v1/responses", Body: []byte(`{"previous_response_id":"r1","input":[]}`), Response: reply("r2", "two")}, {RequestID: "c", StartedAt: now.Add(2 * time.Second), URL: "/v1/responses", Body: []byte(`{"previous_response_id":"r2","input":[{"role":"user","content":"third"}]}`)}}
	if err = ing.IngestRecords(ctx, rs); err != nil {
		t.Fatal(err)
	}
	var depth uint32
	table := "`" + db.Database() + "`."
	err = db.Conn().QueryRow(ctx, "SELECT e.depth FROM "+table+"request_contexts c ARRAY JOIN c.entry_ids AS id INNER JOIN "+table+"transcript_entries e ON e.entry_id=id WHERE c.request_id='c'").Scan(&depth)
	if err != nil || depth != 3 {
		t.Fatalf("depth=%d err=%v", depth, err)
	}
}

func TestUpstreamStreamWithNonStreamingClient(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	ing := New(s, t.TempDir(), 0, 30)
	body := []byte(`{"input":[{"role":"user","content":"hi"}]}`)
	upstream := []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"stream-id\",\"object\":\"response\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"streamed answer\"}]}]}}\n\n")
	r := &record.Request{RequestID: "stream-upstream", StartedAt: time.Now().UTC(), URL: "/v1/responses", Body: body, Attempts: []record.Attempt{{Index: 1, URL: "/v1/responses", Body: body, Response: upstream}}}
	if err = ing.IngestRecords(ctx, []*record.Request{r}); err != nil {
		t.Fatal(err)
	}
	var id string
	err = db.Conn().QueryRow(ctx, "SELECT response_id FROM `"+db.Database()+"`.attempts WHERE request_id='stream-upstream'").Scan(&id)
	if err != nil || id != "stream-id" {
		t.Fatalf("response id %q: %v", id, err)
	}
}

func TestRebuildFetchesContentInOneQuery(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"messages":[{"role":"user","content":"one"},{"role":"assistant","content":"two"},{"role":"user","content":"three"}]}`)
	if err = New(s, t.TempDir(), 0, 30).Process(ctx, &record.Request{RequestID: "bulk-rebuild", StartedAt: time.Now(), URL: "/v1/messages", Body: body}); err != nil {
		t.Fatal(err)
	}
	r, err := store.Request(ctx, db, "bulk-rebuild")
	if err != nil || !bytes.Equal(r.Body, body) {
		t.Fatalf("rebuild %v %v", r, err)
	}
	if err = db.Conn().Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatal(err)
	}
	var n uint64
	err = db.Conn().QueryRow(ctx, "SELECT count() FROM system.query_log WHERE type='QueryFinish' AND startsWith(query,'SELECT ') AND position(query, ?) > 0", "FROM `"+db.Database()+"`.transcript_content FINAL").Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("content fetch used %d queries, expected one: %v", n, err)
	}
}

func TestErrorLogSizeIncludesTranscriptPayload(t *testing.T) {
	ctx := context.Background()
	db := chtest.New(t)
	s, err := store.Open(ctx, db, 30)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("readable payload ", 100)
	body := []byte(fmt.Sprintf(`{"messages":[{"role":"user","content":%q}]}`, text))
	if err = New(s, t.TempDir(), 0, 30).Process(ctx, &record.Request{RequestID: "sized-error", StartedAt: time.Now(), URL: "/v1/messages", Body: body, Detail: "error", Status: 403}); err != nil {
		t.Fatal(err)
	}
	logs, err := store.ErrorLogs(ctx, db)
	if err != nil || len(logs) != 1 || logs[0].Size < int64(len(text)) {
		t.Fatalf("missing transcript payload size: %+v %v", logs, err)
	}
}
