package helps

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGenerationBodyOverflowPreservesBytesAndInvalidatesTiming(t *testing.T) {
	for _, observedFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-first", true: "after-first"}[observedFirst], func(t *testing.T) {
			r := NewUsageReporter(context.Background(), "codex", "test", nil)
			r.StartUpstreamTiming()
			if observedFirst {
				r.ObserveGenerationToken()
			}
			payload := []byte(strings.Repeat(": metadata\n", 40000) + "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
			body := r.TrackGenerationBody(context.Background(), io.NopCloser(bytes.NewReader(payload)), "responses")
			defer func() { _ = body.Close() }()
			// Wait for the independent producer to encounter the full bounded queue.
			deadline := time.After(5 * time.Second)
			for {
				r.ttftMu.RLock()
				invalid := r.generationInvalid
				r.ttftMu.RUnlock()
				if invalid {
					break
				}
				select {
				case <-deadline:
					t.Fatal("producer did not invalidate saturated timing")
				default:
					runtime.Gosched()
				}
			}
			got, err := io.ReadAll(body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("backpressure changed response bytes")
			}
			if r.generationDuration() != nil {
				t.Fatal("saturated timing became measured again")
			}
			first, _, _ := r.localLatencies()
			if (first != nil) != observedFirst {
				t.Fatal("first-token reliability changed")
			}
			r.StartUpstreamTiming()
			r.ObserveGenerationToken()
			if r.generationDuration() == nil {
				t.Fatal("new dispatch retained invalidation")
			}
		})
	}
}

func TestGenerationBodyCancelUnblocksSourceAndConsumer(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	r := NewUsageReporter(ctx, "codex", "test", nil)
	body := r.TrackGenerationBody(ctx, reader, "responses")
	result := make(chan error, 1)
	go func() { _, err := io.ReadAll(body); result <- err }()
	cancel()
	select {
	case <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not unblock read")
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResponsesTerminalOutputEstablishesFirstWithoutExtendingGeneration(t *testing.T) {
	r := NewUsageReporter(context.Background(), "codex", "test", nil)
	r.StartUpstreamTiming()
	at := r.dispatchAt.Add(time.Second)
	done := []byte(`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"hello"}]}}`)
	ObserveResponsesTokenEventAt(r, done, at)
	first, _, _ := r.localLatencies()
	if first == nil || *first != time.Second || r.generationDuration() != nil {
		t.Fatal("terminal-only output needs first observation but no duration")
	}
	delta := []byte(`{"type":"response.output_text.delta","delta":"x"}`)
	ObserveResponsesTokenEventAt(r, delta, at.Add(time.Second))
	ObserveResponsesTokenEventAt(r, delta, at.Add(1200*time.Millisecond))
	ObserveResponsesTokenEventAt(r, done, at.Add(10*time.Second))
	if got := r.generationDuration(); got == nil || *got != 200*time.Millisecond {
		t.Fatalf("terminal snapshot changed duration: %v", got)
	}
}
