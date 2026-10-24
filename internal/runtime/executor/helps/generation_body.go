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
	return r.TrackGenerationBodyObserver(ctx, body, r.generationSSEObserver(protocol))
}

func (r *UsageReporter) generationSSEObserver(protocol string) func([]byte, time.Time, error) {
	r.SetOutputTimingSource(protocol, "source_reader", "visible_deltas")
	var line []byte
	var frameData [][]byte
	frameBytes := 0
	discarding := false
	observeFrame := func(at time.Time) {
		if len(frameData) == 0 {
			return
		}
		payload := bytes.Join(frameData, []byte("\n"))
		r.observeGenerationPayloadAt(protocol, payload, at)
		frameData = nil
		frameBytes = 0
	}
	observeLine := func(value []byte, at time.Time) {
		trimmed := bytes.TrimSpace(value)
		if len(trimmed) == 0 {
			observeFrame(at)
			return
		}
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			data := bytes.TrimSpace(trimmed[len("data:"):])
			if frameBytes+len(data) > generationFrameBytes {
				r.InvalidateGenerationTimingReason("frame_limit")
				frameData = nil
				return
			}
			frameBytes += len(data)
			frameData = append(frameData, bytes.Clone(data))
			return
		}
		if len(frameData) > 0 || bytes.HasPrefix(trimmed, []byte(":")) ||
			bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("id:")) ||
			bytes.HasPrefix(trimmed, []byte("retry:")) {
			return
		}
		r.observeGenerationPayloadAt(protocol, value, at)
	}
	observe := func(data []byte, at time.Time, readErr error) {
		rest := data
		for len(rest) > 0 {
			end := bytes.IndexByte(rest, '\n')
			part := rest
			if end >= 0 {
				part = rest[:end]
			}
			if !discarding {
				if len(line)+len(part) > generationFrameBytes {
					r.InvalidateGenerationTimingReason("frame_limit")
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
				observeLine(line, at)
			}
			line = line[:0]
			discarding = false
			rest = rest[end+1:]
		}
		if readErr != nil && len(line) > 0 && !discarding {
			observeLine(line, at)
			line = line[:0]
		}
		if readErr != nil && !discarding {
			observeFrame(at)
		}
	}
	return observe
}

// TrackGenerationBodyObserver reads independently of downstream delivery and
// supplies one monotonic timestamp for each source read to a protocol observer.
// Observers must retain bounded state and must never change the received bytes.
func (r *UsageReporter) TrackGenerationBodyObserver(ctx context.Context, body io.ReadCloser, observe func([]byte, time.Time, error)) io.ReadCloser {
	if r == nil || body == nil {
		return body
	}
	ctx, cancel := context.WithCancel(ctx)
	b := &generationBody{source: body, cancel: cancel, done: make(chan struct{}), queue: make(chan generationRead, generationReadQueueChunks)}
	go func() { <-ctx.Done(); b.sourceOnce.Do(func() { _ = body.Close() }) }()
	go func() {
		defer close(b.done)
		defer close(b.queue)
		defer cancel()
		for {
			data := make([]byte, generationReadChunkBytes)
			n, err := body.Read(data)
			at := time.Now()
			data = data[:n]
			if observe != nil {
				observe(data, at, err)
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
	trimmed := bytes.TrimSpace(payload)
	if bytes.Equal(trimmed, []byte("[DONE]")) || len(trimmed) == 0 {
		return
	}
	if !gjson.ValidBytes(trimmed) {
		r.InvalidateGenerationTimingReason("malformed_stream")
		return
	}
	text, unsupported := outputDeltaText(protocol, payload)
	if unsupported {
		r.InvalidateGenerationTimingReason("unsupported_protocol")
		return
	}
	if text != "" {
		r.ObserveOutputDeltaAt(text, at)
		return
	}
	if protocol == "responses" {
		r.observeResponsesFirstOutputAt(payload, at)
	}
}

// ObserveResponsesTokenEventAt consumes an arrival captured before any delivery queue.
func ObserveResponsesTokenEventAt(r *UsageReporter, payload []byte, at time.Time) {
	if r == nil {
		return
	}
	r.SetOutputTimingSource("responses", "websocket_reader", "visible_deltas")
	r.observeGenerationPayloadAt("responses", payload, at)
}

func (r *UsageReporter) observeResponsesFirstOutputAt(payload []byte, at time.Time) {
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
