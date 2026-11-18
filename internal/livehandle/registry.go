// Package livehandle tracks active proxy requests and briefly retains their results.
package livehandle

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const Retention = 30 * time.Second
const maxFinished = 200
const maxAttempts = 16

var Default = NewRegistry()

type Key struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}
type Credential struct {
	Label     string `json:"label"`
	AuthIndex string `json:"auth_index"`
}
type Attempt struct {
	Index     int       `json:"index"`
	State     string    `json:"state"`
	StartedAt time.Time `json:"started_at"`
	// AckAt is when the provider first answered: HTTP response headers or a WebSocket frame.
	AckAt          *time.Time `json:"ack_at"`
	FirstOutputAt  *time.Time `json:"first_output_at"`
	LastOutputAt   *time.Time `json:"last_output_at"`
	EndedAt        *time.Time `json:"ended_at"`
	Provider       string     `json:"provider"`
	Credential     Credential `json:"credential"`
	UpstreamModel  string     `json:"upstream_model"`
	OutputBytes    int64      `json:"output_bytes"`
	InputTokens    *int64     `json:"input_tokens"`
	OutputTokens   *int64     `json:"output_tokens"`
	HTTPStatus     int        `json:"http_status"`
	AuthID         string     `json:"-"`
	RequestedModel string     `json:"-"`
	// closedByRequest marks an attempt the request's end closed before its usage arrived.
	closedByRequest bool
}
type Request struct {
	revision uint64
	// shape is the revision of the request's last change other than streamed output growth.
	shape   uint64
	Session string `json:"session,omitempty"`
	// Parent is the digest of the session that spawned this one, such as a subagent's main session.
	Parent         string     `json:"parent,omitempty"`
	ID             string     `json:"id"`
	State          string     `json:"state"`
	ReceivedAt     time.Time  `json:"received_at"`
	EndedAt        *time.Time `json:"ended_at"`
	Endpoint       string     `json:"endpoint"`
	Method         string     `json:"method"`
	Path           string     `json:"path"`
	RequestedModel string     `json:"requested_model"`
	Key            *Key       `json:"key"`
	HTTPStatus     int        `json:"http_status"`
	AttemptCount   int        `json:"attempt_count"`
	// OutputBytes and OutputTokens total every attempt, including ones beyond the attempt window.
	OutputBytes  int64     `json:"output_bytes"`
	OutputTokens *int64    `json:"output_tokens"`
	Attempts     []Attempt `json:"attempts"`
	KeyID        string    `json:"-"`
}
type Snapshot struct {
	ServerTime       time.Time `json:"server_time"`
	RetentionSeconds int       `json:"retention_seconds"`
	Truncated        bool      `json:"truncated"`
	Requests         []Request `json:"requests"`
}
type Registry struct {
	revision   uint64
	streamOnce sync.Once
	stream     *Broadcaster
	mu         sync.Mutex
	requests   map[string]*Request
	// ended holds the finished requests in the map, earliest EndedAt first.
	ended        []*Request
	nowFunc      func() time.Time
	droppedUntil time.Time
}
type Handle struct {
	registry *Registry
	id       string
	index    int
}

func NewRegistry() *Registry {
	return &Registry{requests: make(map[string]*Request), nowFunc: time.Now}
}

// prune drops finished requests past retention, then the earliest-ended beyond the cap.
func (r *Registry) prune(now time.Time) {
	n := 0
	for ; n < len(r.ended); n++ {
		req := r.ended[n]
		expired := !req.EndedAt.Add(Retention).After(now)
		if !expired && len(r.ended)-n <= maxFinished {
			break
		}
		delete(r.requests, req.ID)
		if until := req.EndedAt.Add(Retention); !expired && until.After(r.droppedUntil) {
			r.droppedUntil = until
		}
	}
	// Delete clears the vacated slots, so dropped requests can be collected.
	r.ended = slices.Delete(r.ended, 0, n)
}

// addEnded files a finished request in end order; a clock that stepped back slides it into place.
func (r *Registry) addEnded(req *Request) {
	r.ended = append(r.ended, req)
	for i := len(r.ended) - 1; i > 0 && req.EndedAt.Before(*r.ended[i-1].EndedAt); i-- {
		r.ended[i], r.ended[i-1] = r.ended[i-1], r.ended[i]
	}
}
func (r *Registry) Start(req Request) {
	if req.ID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.nowFunc().UTC()
	r.prune(now)
	if req.ReceivedAt.IsZero() {
		req.ReceivedAt = now
	}
	req.ReceivedAt = req.ReceivedAt.UTC()
	if req.EndedAt != nil {
		// A copy, so the caller cannot move the request within the end order later.
		ended := req.EndedAt.UTC()
		req.EndedAt = &ended
	}
	req.Attempts = []Attempt{}
	r.bump(&req)
	// A reused ID replaces its finished request, which leaves the cap's count too.
	if old := r.requests[req.ID]; old != nil && old.EndedAt != nil {
		if i := slices.Index(r.ended, old); i >= 0 {
			r.ended = slices.Delete(r.ended, i, i+1)
		}
	}
	r.requests[req.ID] = &req
	if req.EndedAt != nil {
		r.addEnded(&req)
	}
}
func (r *Registry) SetModel(id, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req := r.requests[id]; req != nil {
		req.RequestedModel = model
		r.bump(req)
	}
}

// StartAttempt opens an attempt. A non-empty client session ID replaces the request's session and
// its parent; both are kept only as short digests.
func (r *Registry) StartAttempt(id string, a Attempt, sessionID, parentSessionID string) *Handle {
	r.mu.Lock()
	defer r.mu.Unlock()
	req := r.requests[id]
	if req == nil || req.EndedAt != nil {
		return nil
	}
	if session := strings.TrimSpace(sessionID); session != "" {
		req.Session = digest(session)
		req.Parent = ""
		if parent := strings.TrimSpace(parentSessionID); parent != "" && parent != session {
			req.Parent = digest(parent)
		}
	}
	r.bump(req)
	req.AttemptCount++
	a.Index = req.AttemptCount
	a.StartedAt = r.nowFunc().UTC()
	a.State = "waiting"
	req.Attempts = append(req.Attempts, a)
	if len(req.Attempts) > maxAttempts {
		req.Attempts = append([]Attempt(nil), req.Attempts[len(req.Attempts)-maxAttempts:]...)
	}
	return &Handle{r, id, a.Index}
}

// update applies fn to the handle's attempt while it is open, or after the request's end closed it
// when late is true, so usage published after the handler returns still lands.
// With progress, a change that only grew output (after the first output) leaves the request's
// shape alone, so the stream can send it as a compact progress tuple.
func (h *Handle) update(late, progress bool, fn func(*Attempt)) {
	if h == nil {
		return
	}
	r := h.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if req := r.requests[h.id]; req != nil {
		for i := range req.Attempts {
			a := &req.Attempts[i]
			if a.Index == h.index && (a.EndedAt == nil || late && a.closedByRequest) {
				bytes, tokens, state, streamed := a.OutputBytes, a.OutputTokens, a.State, a.FirstOutputAt != nil
				fn(a)
				if progress && streamed && a.State == state {
					r.revision++
					req.revision = r.revision
				} else {
					r.bump(req)
				}
				req.OutputBytes += a.OutputBytes - bytes
				if a.OutputTokens != tokens {
					// A fresh pointer, so snapshots never share a value this lock later changes.
					total := *a.OutputTokens
					if tokens != nil {
						total -= *tokens
					}
					if req.OutputTokens != nil {
						total += *req.OutputTokens
					}
					req.OutputTokens = &total
				}
				return
			}
		}
	}
}

// Ack records the provider's first answer to the attempt; later calls keep the first time.
func (h *Handle) Ack(at time.Time) {
	h.update(false, false, func(a *Attempt) {
		if a.AckAt != nil {
			return
		}
		if at.IsZero() {
			at = time.Now()
		}
		at = at.UTC()
		a.AckAt = &at
	})
}

func (h *Handle) Output(bytes int, at time.Time) {
	h.update(false, true, func(a *Attempt) {
		if at.IsZero() {
			at = time.Now()
		}
		at = at.UTC()
		if a.FirstOutputAt == nil {
			a.FirstOutputAt = &at
		}
		a.LastOutputAt = &at
		a.OutputBytes += int64(bytes)
		a.State = "streaming"
	})
}
func (h *Handle) Finish(outcome string, status int, input, output int64) {
	h.update(true, false, func(a *Attempt) {
		if !a.closedByRequest {
			now := h.registry.nowFunc().UTC()
			a.EndedAt = &now
			a.State = outcome
		}
		a.closedByRequest = false
		a.HTTPStatus = status
		if input != 0 {
			a.InputTokens = &input
		}
		if output != 0 {
			a.OutputTokens = &output
		}
	})
}
func (r *Registry) Finish(id, outcome string, status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.nowFunc().UTC()
	if req := r.requests[id]; req != nil && req.EndedAt == nil {
		// A stream that failed after its 200 header still ends in its last attempt's failure.
		if n := len(req.Attempts); outcome == "finished" && n > 0 && req.Attempts[n-1].State == "failed" {
			outcome = "failed"
		}
		r.bump(req)
		req.EndedAt = &now
		req.State = outcome
		req.HTTPStatus = status
		r.addEnded(req)
		for i := range req.Attempts {
			a := &req.Attempts[i]
			if a.EndedAt == nil {
				a.EndedAt = &now
				a.State = outcome
				a.closedByRequest = true
			}
		}
	}
	r.prune(now)
}

// Snapshot copies the registry's requests. Their time and token pointers still share values with
// the registry, which never changes them in place; callers must treat them as read-only.
func (r *Registry) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.nowFunc().UTC()
	r.prune(now)
	s := Snapshot{ServerTime: now, RetentionSeconds: 30, Truncated: r.droppedUntil.After(now), Requests: make([]Request, 0, len(r.requests))}
	for _, p := range r.requests {
		req := *p
		req.Attempts = append([]Attempt{}, p.Attempts...)
		if req.EndedAt == nil {
			req.State = "routing"
			if len(req.Attempts) > 0 {
				a := req.Attempts[len(req.Attempts)-1]
				if a.EndedAt == nil {
					req.State = a.State
				} else if a.State == "finished" {
					req.State = "idle"
				}
			}
		}
		s.Requests = append(s.Requests, req)
	}
	sort.Slice(s.Requests, func(i, j int) bool { return s.Requests[i].ReceivedAt.Before(s.Requests[j].ReceivedAt) })
	return s
}

func digest(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))[:8] }

func (r *Registry) bump(req *Request) {
	r.revision++
	req.revision = r.revision
	req.shape = r.revision
}
