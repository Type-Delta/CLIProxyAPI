package helps

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/livehandle"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"testing"
	"time"
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
