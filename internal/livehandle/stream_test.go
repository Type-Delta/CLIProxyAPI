package livehandle

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
)

func TestSessionAndRevisions(t *testing.T) {
	r := NewRegistry()
	r.Start(Request{ID: "a"})
	prev := r.Snapshot().Requests[0]
	check := func() Request {
		t.Helper()
		req := r.Snapshot().Requests[0]
		if req.revision <= prev.revision {
			t.Fatalf("revision did not advance: %d <= %d", req.revision, prev.revision)
		}
		prev = req
		return req
	}
	r.SetModel("a", "model")
	check()
	h := r.StartAttempt("a", Attempt{}, "  private session  ", " main session ")
	req := check()
	want := fmt.Sprintf("%x", sha256.Sum256([]byte("private session")))[:8]
	if req.Session != want || req.Parent != fmt.Sprintf("%x", sha256.Sum256([]byte("main session")))[:8] {
		t.Fatalf("session %q parent %q", req.Session, req.Parent)
	}
	h.Output(1, time.Now())
	check()
	h.Finish("finished", 200, 1, 1)
	check()
	r.StartAttempt("a", Attempt{}, " ", "other")
	if req := check(); req.Session != want || req.Parent == "" {
		t.Fatal("empty overwrote session")
	}
	r.StartAttempt("a", Attempt{}, "new session", "")
	if req := check(); req.Session == want || req.Parent != "" {
		t.Fatal("latest session lost or kept the old parent")
	}
	r.Finish("a", "finished", 200)
	req = check()
	frame := string(encodeEvent(r.Snapshot(), true, nil, nil))
	if strings.Contains(frame, "private session") || strings.Contains(frame, "main session") || strings.Contains(frame, "revision") {
		t.Fatal(frame)
	}
	if req.revision != r.revision {
		t.Fatal("registry revision mismatch")
	}
	r.Start(Request{ID: "b"})
	for _, req := range r.Snapshot().Requests {
		if req.ID == "b" && req.Session != "" {
			t.Fatal(req.Session)
		}
	}
}

func testBroadcaster(r *Registry) (*Broadcaster, chan time.Time) {
	ticks := make(chan time.Time)
	return &Broadcaster{registry: r, subscribers: make(map[chan []byte]struct{}), newTicker: func() (<-chan time.Time, func()) { return ticks, func() {} }}, ticks
}
func eventRequests(frame []byte) []ojson.Result {
	return ojson.GetBytes(frame[6:len(frame)-2], "requests").Array()
}

func TestBroadcasterDeltasAndConsistency(t *testing.T) {
	r := NewRegistry()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Clock reads and test mutations are synchronized by subscription or event delivery.
	clockReads := 0
	r.nowFunc = func() time.Time { clockReads++; return now }
	r.Start(Request{ID: "a"})
	b, ticks := testBroadcaster(r)
	one, offOne := b.subscribe(nil)
	defer offOne()
	full := <-one
	r.Start(Request{ID: "b"})
	readsBeforeSubscribe := clockReads
	two, offTwo := b.subscribe(nil)
	defer offTwo()
	if got := <-two; !bytes.Equal(full, got) || &full[0] != &got[0] || clockReads != readsBeforeSubscribe {
		t.Fatalf("full changed between ticks: %s %s", full, got)
	}
	if len(eventRequests(full)) != 1 || !bytes.Contains(full, []byte(`"full":true`)) {
		t.Fatal(string(full))
	}
	tick := func() []byte {
		t.Helper()
		readsBeforeTick := clockReads
		ticks <- now
		a, c := <-one, <-two
		if clockReads != readsBeforeTick+1 {
			t.Fatal("tick did not take exactly one snapshot")
		}
		if !bytes.Equal(a, c) || &a[0] != &c[0] {
			t.Fatal("events not shared")
		}
		return a
	}
	frame := tick()
	if reqs := eventRequests(frame); len(reqs) != 1 || reqs[0].Get("id").String() != "b" {
		t.Fatal(string(frame))
	}
	if reqs := eventRequests(tick()); len(reqs) != 0 {
		t.Fatal("unchanged requests resent")
	}
	r.SetModel("a", "changed")
	if reqs := eventRequests(tick()); len(reqs) != 1 || reqs[0].Get("requested_model").String() != "changed" {
		t.Fatal(reqs)
	}
	r.Finish("a", "finished", 200)
	tick()
	now = now.Add(Retention)
	frame = tick()
	if !bytes.Contains(frame, []byte(`"removed":["a"]`)) || len(eventRequests(frame)) != 0 {
		t.Fatal(string(frame))
	}
	offOne()
	offTwo()
	<-b.done
	b.mu.Lock()
	count := len(b.subscribers)
	b.mu.Unlock()
	if count != 0 {
		t.Fatal(count)
	}
	// Restart after idle takes a fresh snapshot, without relying on another tick.
	r.Start(Request{ID: "c"})
	three, offThree := b.subscribe(nil)
	if len(eventRequests(<-three)) != 2 {
		t.Fatal("idle snapshot stale")
	}
	offThree()
	<-b.done
}

func TestBroadcasterDropsSlowSubscriber(t *testing.T) {
	b, ticks := testBroadcaster(NewRegistry())
	slow, offSlow := b.subscribe(nil)
	defer offSlow()
	fast, offFast := b.subscribe(nil)
	defer offFast()
	<-fast
	for i := 0; i < subscriberBuffer; i++ {
		ticks <- time.Time{}
		<-fast
	}
	// The initial full plus three ticks filled slow's queue; the fourth drops it.
	for range slow {
	}
	b.mu.Lock()
	count := len(b.subscribers)
	b.mu.Unlock()
	if count != 1 {
		t.Fatal(count)
	}
	offFast()
	<-b.done
}

func TestBroadcasterLabelChanges(t *testing.T) {
	r := NewRegistry()
	r.Start(Request{ID: "a"})
	b, ticks := testBroadcaster(r)
	label := "before"
	frames, off := b.subscribe(func(s *Snapshot) { s.Requests[0].Key = &Key{ID: "short", Label: label} })
	defer off()
	<-frames
	label = "after"
	ticks <- time.Time{}
	if reqs := eventRequests(<-frames); len(reqs) != 1 || reqs[0].Get("key.label").String() != "after" {
		t.Fatal(reqs)
	}
	off()
	<-b.done
}

func TestBroadcasterSendsOutputGrowthAsProgress(t *testing.T) {
	r := NewRegistry()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.nowFunc = func() time.Time { return now }
	r.Start(Request{ID: "a"})
	h := r.StartAttempt("a", Attempt{Provider: "codex"}, "", "")
	b, ticks := testBroadcaster(r)
	events, off := b.subscribe(nil)
	defer off()
	<-events
	tick := func() []byte {
		t.Helper()
		ticks <- now
		frame := <-events
		return frame[6 : len(frame)-2]
	}

	// The first output turns the attempt to streaming, so the whole request goes out.
	h.Output(100, now)
	frame := tick()
	if len(ojson.GetBytes(frame, "requests").Array()) != 1 || ojson.GetBytes(frame, "progress").Exists() {
		t.Fatalf("first output: %s", frame)
	}
	// Later growth is a compact tuple: id, request bytes, attempt, attempt bytes, last output.
	h.Output(50, now.Add(time.Second))
	h.Output(25, now.Add(2*time.Second))
	frame = tick()
	if got := ojson.GetBytes(frame, "requests").Array(); len(got) != 0 {
		t.Fatalf("growth resent the request: %s", frame)
	}
	want := `[["a",175,1,175,"2026-01-01T00:00:02Z"]]`
	if got := ojson.GetBytes(frame, "progress").Raw; got != want {
		t.Fatalf("progress %s, want %s", got, want)
	}
	if ojson.GetBytes(tick(), "progress").Exists() {
		t.Fatal("unchanged request sent progress")
	}
	// Ending the attempt changes its shape again.
	h.Finish("finished", 200, 10, 44)
	frame = tick()
	if got := ojson.GetBytes(frame, "requests").Array(); len(got) != 1 || got[0].Get("attempts.0.output_tokens").Int() != 44 {
		t.Fatalf("finish: %s", frame)
	}
}

func TestBroadcasterResendsRenamedCredentials(t *testing.T) {
	r := NewRegistry()
	r.Start(Request{ID: "a"})
	r.StartAttempt("a", Attempt{AuthID: "auth-1"}, "", "")
	label := "Before"
	b, ticks := testBroadcaster(r)
	events, off := b.subscribe(func(s *Snapshot) {
		for i := range s.Requests {
			for j := range s.Requests[i].Attempts {
				s.Requests[i].Attempts[j].Credential.Label = label
			}
		}
	})
	defer off()
	<-events
	tick := func() []ojson.Result {
		ticks <- time.Now()
		frame := <-events
		return ojson.GetBytes(frame[6:len(frame)-2], "requests").Array()
	}
	if len(tick()) != 0 {
		t.Fatal("unchanged request resent")
	}
	// A rename changes no registry revision; the decorated label alone must go out.
	label = "After"
	got := tick()
	if len(got) != 1 || got[0].Get("attempts.0.credential.label").String() != "After" {
		t.Fatalf("rename not sent: %v", got)
	}
}

func TestAckKeepsTheFirstAnswer(t *testing.T) {
	r := NewRegistry()
	r.Start(Request{ID: "a"})
	h := r.StartAttempt("a", Attempt{}, "", "")
	before := r.Snapshot().Requests[0]
	first := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	h.Ack(first)
	h.Ack(first.Add(time.Second))
	req := r.Snapshot().Requests[0]
	if a := req.Attempts[0]; a.AckAt == nil || !a.AckAt.Equal(first) || a.State != "waiting" {
		t.Fatalf("attempt %+v", a)
	}
	// An acknowledgement changes what the request shows, so it is sent whole, not as progress.
	if req.shape <= before.shape {
		t.Fatal("ack did not change the request's shape")
	}
	if frame := string(encodeEvent(r.Snapshot(), true, nil, nil)); !strings.Contains(frame, `"ack_at":"2026-01-01T00:00:01Z"`) {
		t.Fatal(frame)
	}
}
