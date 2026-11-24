package livehandle

import (
	"sort"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
)

const subscriberBuffer = 4

// Broadcaster shares snapshot collection and event encoding across subscribers.
// Its mutex serializes subscription with ticks, so a cached full always precedes
// precisely the deltas that follow it.
type Broadcaster struct {
	mu          sync.Mutex
	registry    *Registry
	decorate    func(*Snapshot)
	newTicker   func() (<-chan time.Time, func())
	subscribers map[chan []byte]struct{}
	previous    map[string]Request
	snapshot    Snapshot
	full        []byte
	stop        chan struct{}
	done        chan struct{}
}

// Subscribe returns SSE frames and an idempotent unsubscribe function. Frames
// are shared immutable bytes; consumers must not modify them.
func (r *Registry) Subscribe(decorate func(*Snapshot)) (<-chan []byte, func()) {
	r.streamOnce.Do(func() {
		r.stream = &Broadcaster{registry: r, subscribers: make(map[chan []byte]struct{}), newTicker: func() (<-chan time.Time, func()) {
			ticker := time.NewTicker(time.Second)
			return ticker.C, ticker.Stop
		}}
	})
	return r.stream.subscribe(decorate)
}

func (b *Broadcaster) subscribe(decorate func(*Snapshot)) (<-chan []byte, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.decorate = decorate
	ch := make(chan []byte, subscriberBuffer)
	if len(b.subscribers) == 0 {
		b.collect()
	}
	if b.full == nil {
		b.full = encodeEvent(b.snapshot, true, nil, nil)
	}
	ch <- b.full
	b.subscribers[ch] = struct{}{}
	if len(b.subscribers) == 1 {
		b.stop = make(chan struct{})
		b.done = make(chan struct{})
		ticks, stopTicker := b.newTicker()
		go b.run(ticks, stopTicker, b.stop, b.done)
	}
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.remove(ch)
	}
}

func (b *Broadcaster) remove(ch chan []byte) {
	if _, ok := b.subscribers[ch]; !ok {
		return
	}
	delete(b.subscribers, ch)
	close(ch)
	if len(b.subscribers) == 0 {
		close(b.stop)
	}
}

func (b *Broadcaster) run(ticks <-chan time.Time, stopTicker func(), stop, done chan struct{}) {
	defer close(done)
	defer stopTicker()
	for {
		select {
		case <-stop:
			return
		case <-ticks:
			b.mu.Lock()
			// A new subscription may already have started a different generation.
			if b.stop != stop || len(b.subscribers) == 0 {
				b.mu.Unlock()
				return
			}
			delta, progress, removed := b.collect()
			frame := encodeEvent(delta, false, progress, removed)
			for ch := range b.subscribers {
				select {
				case ch <- frame:
				default:
					b.remove(ch)
				}
			}
			b.mu.Unlock()
		}
	}
}

// Progress is streamed output growth on one attempt of a request whose shape is unchanged. It
// encodes as [id, request output bytes, attempt index, attempt output bytes, last output time].
type Progress struct {
	ID                 string
	OutputBytes        int64
	Attempt            int
	AttemptOutputBytes int64
	LastOutputAt       *time.Time
}

func (p Progress) tuple() []any {
	return []any{p.ID, p.OutputBytes, p.Attempt, p.AttemptOutputBytes, p.LastOutputAt}
}

// grown lists the attempts of req whose output grew since prev.
func grown(prev, req Request) []Progress {
	before := make(map[int]int64, len(prev.Attempts))
	for _, a := range prev.Attempts {
		before[a.Index] = a.OutputBytes
	}
	var out []Progress
	for _, a := range req.Attempts {
		if bytes, ok := before[a.Index]; !ok || bytes != a.OutputBytes {
			out = append(out, Progress{req.ID, req.OutputBytes, a.Index, a.OutputBytes, a.LastOutputAt})
		}
	}
	return out
}

// relabeled reports display names the decorator changed without a registry revision: a renamed
// API key or credential.
func relabeled(prev, req Request) bool {
	if (prev.Key == nil) != (req.Key == nil) || prev.Key != nil && *prev.Key != *req.Key {
		return true
	}
	if len(prev.Attempts) != len(req.Attempts) {
		return false // a new attempt advanced the shape anyway
	}
	for i := range req.Attempts {
		if prev.Attempts[i].Credential != req.Attempts[i].Credential {
			return true
		}
	}
	return false
}

// collect runs under mu and takes exactly one registry snapshot. Requests whose only change is
// streamed output come back as progress instead of whole requests.
func (b *Broadcaster) collect() (Snapshot, []Progress, []string) {
	snapshot := b.registry.Snapshot()
	if b.decorate != nil {
		b.decorate(&snapshot)
	}
	delta := snapshot
	delta.Requests = make([]Request, 0)
	next := make(map[string]Request, len(snapshot.Requests))
	var progress []Progress
	for _, req := range snapshot.Requests {
		prev, exists := b.previous[req.ID]
		switch {
		case !exists || prev.shape != req.shape || relabeled(prev, req):
			delta.Requests = append(delta.Requests, req)
		case prev.revision != req.revision:
			progress = append(progress, grown(prev, req)...)
		}
		next[req.ID] = req
	}
	var removed []string
	for id := range b.previous {
		if _, ok := next[id]; !ok {
			removed = append(removed, id)
		}
	}
	sort.Strings(removed)
	b.previous = next
	b.snapshot = snapshot
	b.full = nil
	return delta, progress, removed
}

func encodeEvent(snapshot Snapshot, full bool, progress []Progress, removed []string) []byte {
	doc := ojson.NewDocString("{}")
	// These values contain only JSON-supported types and registry-generated times.
	if full {
		_ = doc.Set("full", true)
	}
	_ = doc.Set("server_time", snapshot.ServerTime)
	_ = doc.Set("retention_seconds", snapshot.RetentionSeconds)
	_ = doc.Set("truncated", snapshot.Truncated)
	_ = doc.Set("requests", snapshot.Requests)
	if len(progress) > 0 {
		tuples := make([][]any, len(progress))
		for i, p := range progress {
			tuples[i] = p.tuple()
		}
		_ = doc.Set("progress", tuples)
	}
	if len(removed) > 0 {
		_ = doc.Set("removed", removed)
	}
	frame := append([]byte("data: "), doc.Bytes()...)
	return append(frame, '\n', '\n')
}
