package livehandle

import (
	"fmt"
	"testing"
	"time"
)

func TestStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Registry)
		want string
	}{
		{"routing", func(*Registry) {}, "routing"},
		{"waiting", func(r *Registry) { r.StartAttempt("id", Attempt{}, "", "") }, "waiting"},
		{"streaming", func(r *Registry) { r.StartAttempt("id", Attempt{}, "", "").Output(2, time.Now()) }, "streaming"},
		{"idle", func(r *Registry) { r.StartAttempt("id", Attempt{}, "", "").Finish("finished", 200, 0, 0) }, "idle"},
		{"retry", func(r *Registry) { r.StartAttempt("id", Attempt{}, "", "").Finish("failed", 500, 0, 0) }, "routing"},
		{"cancelled", func(r *Registry) { r.Finish("id", "cancelled", 0) }, "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			r.Start(Request{ID: "id"})
			tc.run(r)
			if got := r.Snapshot().Requests[0].State; got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}
func TestCapsAndRetention(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := NewRegistry()
	r.nowFunc = func() time.Time { return now }
	r.Start(Request{ID: "active"})
	for i := 0; i < 20; i++ {
		r.StartAttempt("active", Attempt{}, "", "").Finish("finished", 0, 0, 0)
	}
	req := r.Snapshot().Requests[0]
	if req.AttemptCount != 20 || len(req.Attempts) != 16 || req.Attempts[0].Index != 5 {
		t.Fatalf("attempt cap: %+v", req)
	}
	for i := 0; i < 201; i++ {
		id := fmt.Sprint(i)
		r.Start(Request{ID: id})
		r.Finish(id, "finished", 200)
		now = now.Add(time.Millisecond)
	}
	s := r.Snapshot()
	if len(s.Requests) != 201 || !s.Truncated {
		t.Fatalf("cap: %d truncated %v", len(s.Requests), s.Truncated)
	}
	now = now.Add(Retention)
	s = r.Snapshot()
	if len(s.Requests) != 1 || s.Truncated {
		t.Fatalf("retention: %+v", s)
	}
}

func TestLateUsageAfterRequestEnd(t *testing.T) {
	r := NewRegistry()
	r.Start(Request{ID: "id"})
	h := r.StartAttempt("id", Attempt{}, "", "")
	h.Output(40, time.Now())
	r.Finish("id", "finished", 200)
	h.Finish("finished", 200, 12, 34)
	a := r.Snapshot().Requests[0].Attempts[0]
	if a.State != "finished" || a.OutputTokens == nil || *a.OutputTokens != 34 || a.InputTokens == nil || *a.InputTokens != 12 {
		t.Fatalf("late usage lost: %+v", a)
	}
	h.Finish("failed", 500, 1, 1)
	if a := r.Snapshot().Requests[0].Attempts[0]; *a.OutputTokens != 34 {
		t.Fatalf("second finish overwrote attempt: %+v", a)
	}
}

func TestStreamFailureAfterHeadersFailsRequest(t *testing.T) {
	r := NewRegistry()
	r.Start(Request{ID: "id"})
	r.StartAttempt("id", Attempt{}, "", "").Finish("failed", 502, 0, 0)
	r.Finish("id", "finished", 200)
	if got := r.Snapshot().Requests[0].State; got != "failed" {
		t.Fatalf("got %s want failed", got)
	}
}

func TestRequestTotalsOutliveTheAttemptWindow(t *testing.T) {
	r := NewRegistry()
	r.Start(Request{ID: "id"})
	for i := 0; i < maxAttempts+1; i++ {
		h := r.StartAttempt("id", Attempt{}, "", "")
		h.Output(100, time.Now())
		h.Finish("finished", 200, 0, 25)
	}
	req := r.Snapshot().Requests[0]
	if len(req.Attempts) != maxAttempts || req.AttemptCount != maxAttempts+1 {
		t.Fatalf("attempt window: kept %d of %d", len(req.Attempts), req.AttemptCount)
	}
	if req.OutputBytes != 1700 || req.OutputTokens == nil || *req.OutputTokens != 425 {
		t.Fatalf("request totals lost dropped turns: bytes=%d tokens=%v", req.OutputBytes, req.OutputTokens)
	}
}
