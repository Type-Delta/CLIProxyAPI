package helps

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/livehandle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestLiveHandleReporter(t *testing.T) {
	ctx, id := usage.EnsureProxyRequestID(context.Background())
	ctx = logging.WithClientRequestMetadata(ctx, logging.ClientRequestMetadata{SessionID: "  reporter-session  "})
	livehandle.Default.Start(livehandle.Request{ID: id})
	defer livehandle.Default.Finish(id, "finished", 200)
	r := NewUsageReporter(ctx, "codex", "model", nil)
	r.StartUpstreamTiming()
	r.ObserveUpstreamResponse()
	r.ObserveOutputDeltaAt("hello", time.Now())
	r.publishWithOutcome(ctx, usage.Detail{InputTokens: 3, OutputTokens: 4}, false, usage.Failure{})
	for _, req := range livehandle.Default.Snapshot().Requests {
		if req.ID == id {
			if len(req.Attempts) != 1 {
				t.Fatal(req)
			}
			if want := fmt.Sprintf("%x", sha256.Sum256([]byte("reporter-session")))[:8]; req.Session != want {
				t.Fatalf("session %q != %q", req.Session, want)
			}
			a := req.Attempts[0]
			if a.State != "finished" || a.AckAt == nil || a.OutputBytes != 5 || a.FirstOutputAt == nil || a.InputTokens == nil || *a.InputTokens != 3 {
				t.Fatalf("attempt: %+v", a)
			}
			return
		}
	}
	t.Fatal("request missing")
}

func TestLiveHandleAckFromWebsocketFrame(t *testing.T) {
	ctx, id := usage.EnsureProxyRequestID(context.Background())
	livehandle.Default.Start(livehandle.Request{ID: id})
	defer livehandle.Default.Finish(id, "finished", 200)
	r := NewUsageReporter(ctx, "codex", "model", nil)
	r.StartUpstreamTiming()
	frame := time.Now().Add(time.Second).UTC()
	r.ObserveUpstreamResponseAt(frame)
	for _, req := range livehandle.Default.Snapshot().Requests {
		if req.ID == id {
			if a := req.Attempts[0]; a.AckAt == nil || !a.AckAt.Equal(frame) {
				t.Fatalf("attempt %+v", a)
			}
			return
		}
	}
	t.Fatal("request missing")
}

func TestLiveHandleCountsResponsesToolCalls(t *testing.T) {
	ctx, id := usage.EnsureProxyRequestID(t.Context())
	livehandle.Default.Start(livehandle.Request{ID: id})
	defer livehandle.Default.Finish(id, "finished", 200)
	reporter := NewUsageReporter(ctx, "codex", "model", nil)

	stream := "data: " + `{"type":"response.output_item.added","output_index":1,"item":{"type":"web_search_call"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","output_index":1,"item":{"type":"web_search_call","id":"ws_1"}}` + "\n\n"
	body := reporter.TrackGenerationBody(ctx, io.NopCloser(strings.NewReader(stream)), "responses")
	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot := func() livehandle.Request {
		for _, req := range livehandle.Default.Snapshot().Requests {
			if req.ID == id {
				return req
			}
		}
		t.Fatal("request missing")
		return livehandle.Request{}
	}
	if got := snapshot().ToolCalls; got != 1 {
		t.Fatalf("live HTTP count = %d, want 1", got)
	}

	ObserveResponsesTokenEventAt(reporter, []byte(`{"type":"response.completed","response":{"output":[{"type":"web_search_call","id":"ws_1"},{"type":"function_call","id":"fc_1"},{"type":"function_call_output"},{"type":"message"}]}}`), time.Now())
	reporter.liveAttempt.Finish("finished", 200, 0, 0)
	next := NewUsageReporter(ctx, "codex", "model", nil)
	ObserveResponsesTokenEventAt(next, []byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call","id":"ws_2"}}`), time.Now())
	req := snapshot()
	if req.ToolCalls != 3 || req.Attempts[0].ToolCalls != 2 || req.Attempts[1].ToolCalls != 1 {
		t.Fatalf("tool counts = %d across attempts %+v", req.ToolCalls, req.Attempts)
	}
}
