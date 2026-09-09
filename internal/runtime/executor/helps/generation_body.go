package helps

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const generationReadChunkBytes = 16 * 1024
const generationReadQueueChunks = 16
const generationFrameBytes = 1024 * 1024

type generationRead struct {
	data []byte
	err  error
}
type generationBody struct {
	source     io.ReadCloser
	cancel     context.CancelFunc
	queue      chan generationRead
	pending    []byte
	pendingErr error
	done       chan struct{}
	once       sync.Once
	sourceOnce sync.Once
}

// TrackGenerationBody observes SSE arrivals before downstream processing. Memory
// is bounded; queue pressure preserves payloads but invalidates timing.
func (r *UsageReporter) TrackGenerationBody(ctx context.Context, body io.ReadCloser, protocol string) io.ReadCloser {
	if r == nil || body == nil {
		return body
	}
	r.ttftMu.Lock()
	r.readerOwned = true
	r.ttftMu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	b := &generationBody{source: body, cancel: cancel, done: make(chan struct{}), queue: make(chan generationRead, generationReadQueueChunks)}
	go func() { <-ctx.Done(); b.sourceOnce.Do(func() { _ = body.Close() }) }()
	go func() {
		defer close(b.done)
		defer close(b.queue)
		defer cancel()
		var line []byte
		discarding := false
		for {
			data := make([]byte, generationReadChunkBytes)
			n, err := body.Read(data)
			at := time.Now()
			data = data[:n]
			rest := data
			for len(rest) > 0 {
				end := bytes.IndexByte(rest, '\n')
				part := rest
				if end >= 0 {
					part = rest[:end]
				}
				if !discarding {
					if len(line)+len(part) > generationFrameBytes {
						r.InvalidateGenerationTiming()
						line = nil
						discarding = true
					} else {
						line = append(line, part...)
					}
				}
				if end < 0 {
					break
				}
				if !discarding {
					r.observeGenerationPayloadAt(protocol, line, at)
				}
				line = line[:0]
				discarding = false
				rest = rest[end+1:]
			}
			if err != nil && len(line) > 0 && !discarding {
				r.observeGenerationPayloadAt(protocol, line, at)
			}
			item := generationRead{data: data, err: err}
			select {
			case b.queue <- item:
			default:
				r.InvalidateGenerationTiming()
				select {
				case b.queue <- item:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()
	return b
}
func (b *generationBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 && b.pendingErr == nil {
		item, ok := <-b.queue
		if !ok {
			return 0, io.EOF
		}
		b.pending = item.data
		b.pendingErr = item.err
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	if len(b.pending) == 0 {
		err := b.pendingErr
		b.pendingErr = nil
		return n, err
	}
	return n, nil
}
func (b *generationBody) Close() error {
	var err error
	b.once.Do(func() { b.cancel(); b.sourceOnce.Do(func() { err = b.source.Close() }); <-b.done })
	return err
}
func (r *UsageReporter) readerOwnsGeneration() bool {
	if r == nil {
		return false
	}
	r.ttftMu.RLock()
	defer r.ttftMu.RUnlock()
	return r.readerOwned
}

func (r *UsageReporter) observeGenerationPayloadAt(protocol string, payload []byte, at time.Time) {
	switch protocol {
	case "responses":
		ObserveResponsesTokenEventAt(r, payload, at)
	case "claude":
		if isClaudeTokenEvent(payload, false) {
			r.ObserveGenerationTokenAt(at)
		}
	case "gemini":
		if isGeminiTokenEvent(payload, false) {
			r.ObserveGenerationTokenAt(at)
		}
	default:
		if isChatTokenEvent(payload, false) {
			r.ObserveGenerationTokenAt(at)
		}
	}
}

// ObserveResponsesTokenEventAt consumes an arrival captured before any delivery queue.
func ObserveResponsesTokenEventAt(r *UsageReporter, payload []byte, at time.Time) {
	if r == nil {
		return
	}
	if isResponsesTokenEvent(payload, false) {
		r.ObserveGenerationTokenAt(at)
		return
	}
	payload = JSONPayload(payload)
	event := gjson.GetBytes(payload, "type").String()
	// Completed snapshots can establish first visible output, never a duration.
	visible := false
	if (strings.HasSuffix(event, ".done") && event != "response.done") || event == "response.output_item.added" {
		visible = isResponsesTokenEvent(payload, true)
	}
	if event == "response.completed" || event == "response.incomplete" || event == "response.done" {
		for _, item := range gjson.GetBytes(payload, "response.output").Array() {
			if len(item.Get("arguments").String()) > 0 || len(item.Get("input").String()) > 0 {
				visible = true
			}
			for _, content := range item.Get("content").Array() {
				if len(content.Get("text").String()) > 0 || len(content.Get("refusal").String()) > 0 {
					visible = true
				}
			}
		}
	}
	if visible {
		r.observeFirstOutputAt(at)
	}
}
func (r *UsageReporter) observeFirstOutputAt(at time.Time) {
	r.ttftMu.Lock()
	defer r.ttftMu.Unlock()
	if r.generationInvalid || r.firstTokenLatency != nil || r.dispatchAt.IsZero() {
		return
	}
	elapsed := at.Sub(r.dispatchAt)
	r.firstTokenLatency = &elapsed
	if !r.ttftSet && !r.ttftStart.IsZero() {
		r.ttft = at.Sub(r.ttftStart)
		r.ttftSet = true
		r.ttftStart = time.Time{}
	}
}
