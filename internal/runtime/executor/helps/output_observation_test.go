package helps

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestOutputTimingExcludesEntireFirstReadBatch(t *testing.T) {
	r := NewUsageReporter(context.Background(), "openai", "gpt-4o", nil)
	r.StartUpstreamTiming()
	r.SetOutputTimingSource("chat", "source_reader", "visible_deltas")
	first := r.dispatchAt.Add(20 * time.Second)
	r.ObserveOutputDeltaAt("Hello", first)
	r.ObserveOutputDeltaAt(" world", first)
	r.ObserveOutputDeltaAt(strings.Repeat(" token", 100), first.Add(100*time.Millisecond))
	r.ObserveOutputDeltaAt(strings.Repeat(" token", 100), first.Add(200*time.Millisecond))
	got := r.outputTiming()
	encoder, err := TokenizerForModel(r.model)
	if err != nil {
		t.Fatal(err)
	}
	prefix, _ := encoder.Count("Hello world")
	final, _ := encoder.Count("Hello world" + strings.Repeat(" token", 200))
	if got.Status != "observed" || got.ArrivalCount != 3 || got.WindowTokens != int64(final-prefix) || got.TokenQuality != usage.TokenQualityEstimated {
		t.Fatalf("matching first-batch window = %+v", got)
	}
	if got.FirstArrivalNS != int64(20*time.Second) || got.LastArrivalNS-got.FirstArrivalNS != int64(200*time.Millisecond) {
		t.Fatalf("queue wait changed window: %+v", got)
	}
	if got.FirstBatchBytes != 11 || got.TotalBytes != 1211 || got.LargestBatchBytes != 600 || got.MaxInterarrivalNS != int64(100*time.Millisecond) {
		t.Fatalf("source batch statistics = %+v", got)
	}
	// Arrival rates have no speed ceiling. The same matching content can arrive
	// over a 100ms window even after a long queue wait.
	r.ttftMu.Lock()
	r.outputObservation.last = first.Add(100 * time.Millisecond)
	r.ttftMu.Unlock()
	fast := r.outputTiming()
	if float64(fast.WindowTokens)/0.1 < 1500 || fast.Status != "observed" {
		t.Fatalf("future fast stream suppressed: %+v", fast)
	}
}

func TestOutputTimingCountsCumulativeContentInsteadOfFragments(t *testing.T) {
	r := NewUsageReporter(context.Background(), "openai", "gpt-4o", nil)
	r.StartUpstreamTiming()
	r.SetOutputTimingSource("chat", "source_reader", "visible_deltas")
	at := r.dispatchAt.Add(time.Second)
	parts := []string{"a ", "est", "ablish", "ment", " testing"}
	for i, part := range parts {
		r.ObserveOutputDeltaAt(part, at.Add(time.Duration(i)*100*time.Millisecond))
	}
	encoder, _ := TokenizerForModel(r.model)
	first, _ := encoder.Count(parts[0])
	all, _ := encoder.Count(strings.Join(parts, ""))
	if got := r.outputTiming(); got.WindowTokens != int64(all-first) {
		t.Fatalf("token boundaries counted separately: %+v", got)
	}
}

func TestGenerationBodyOneBufferedReadIsOneArrival(t *testing.T) {
	r := NewUsageReporter(context.Background(), "openai", "gpt-4o", nil)
	r.StartUpstreamTiming()
	payload := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\" second\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\" third\"}}]}\n\n")
	body := r.TrackGenerationBody(context.Background(), io.NopCloser(bytes.NewReader(payload)), "chat")
	defer body.Close()
	gotBytes, err := io.ReadAll(body)
	if err != nil || !bytes.Equal(gotBytes, payload) {
		t.Fatalf("source bytes changed: err=%v", err)
	}
	got := r.outputTiming()
	if got.ArrivalCount != 1 || got.Status != "unavailable" || got.Reason != "insufficient_arrivals" {
		t.Fatalf("buffered frames masquerade as separate arrivals: %+v", got)
	}
}

func TestOutputTimingHiddenUsageDoesNotEnterVisibleNumerator(t *testing.T) {
	r := NewUsageReporter(context.Background(), "codex", "gpt-4o", nil)
	r.StartUpstreamTiming()
	at := r.dispatchAt.Add(time.Second)
	ObserveResponsesTokenEventAt(r, []byte(`{"type":"response.output_text.delta","delta":"Hello"}`), at)
	ObserveResponsesTokenEventAt(r, []byte(`{"type":"response.output_text.delta","delta":" visible text"}`), at.Add(time.Second))
	before := r.outputTiming()
	detail := usage.Detail{OutputTokens: 10000, ReasoningTokens: 9990}
	record := r.buildRecord(detail, false)
	if record.OutputTiming.WindowTokens != before.WindowTokens || record.OutputTiming.Coverage != "visible_deltas" {
		t.Fatalf("hidden reasoning affected matching numerator: %+v", record.OutputTiming)
	}
	ObserveResponsesTokenEventAt(r, []byte(`{"type":"response.completed","response":{"output":[],"usage":{"output_tokens":10000}}}`), at.Add(10*time.Second))
	if got := r.outputTiming(); *got != *before {
		t.Fatalf("terminal usage changed arrival window: before=%+v after=%+v", before, got)
	}
}

func TestOutputTimingBoundedCoverageAndRetryReset(t *testing.T) {
	r := NewUsageReporter(context.Background(), "openai", "gpt-4o", nil)
	r.StartUpstreamTiming()
	r.SetOutputTimingSource("chat", "source_reader", "visible_deltas")
	at := r.dispatchAt.Add(time.Second)
	r.ObserveOutputDeltaAt("a", at)
	r.ObserveOutputDeltaAt(strings.Repeat("b", outputObservationBytes), at.Add(time.Second))
	r.ObserveOutputDeltaAt("still observed", at.Add(3*time.Second))
	if got := r.outputTiming(); got.Status != "unavailable" || got.Reason != "coverage_limit" || len(r.outputObservation.content) != 0 {
		t.Fatalf("collector cap ignored: %+v", got)
	}
	if got := r.outputTiming(); got.FirstBatchBytes != 1 || got.TotalBytes != outputObservationBytes+15 || got.LargestBatchBytes != outputObservationBytes || got.MaxInterarrivalNS != int64(2*time.Second) || got.ArrivalCount != 3 {
		t.Fatalf("coverage cap erased source statistics: %+v", got)
	}
	r.StartUpstreamTiming()
	if got := r.outputTiming(); got.ArrivalCount != 0 || got.Reason != "unsupported_protocol" {
		t.Fatalf("retry retained previous evidence: %+v", got)
	}
	if got := r.outputTiming(); got.FirstBatchBytes != 0 || got.TotalBytes != 0 || got.LargestBatchBytes != 0 || got.MaxInterarrivalNS != 0 {
		t.Fatalf("retry retained batch statistics: %+v", got)
	}
}

func TestOutputTimingBatchStatisticsCountUTF8BytesAndEntireFirstBatch(t *testing.T) {
	r := NewUsageReporter(context.Background(), "openai", "gpt-4o", nil)
	r.StartUpstreamTiming()
	r.SetOutputTimingSource("chat", "source_reader", "visible_deltas")
	at := r.dispatchAt.Add(time.Second)
	r.ObserveOutputDeltaAt("é", at)
	r.ObserveOutputDeltaAt("abc", at)
	r.ObserveOutputDeltaAt("123456", at.Add(100*time.Millisecond))
	r.ObserveOutputDeltaAt("789", at.Add(100*time.Millisecond))
	r.ObserveOutputDeltaAt("last", at.Add(400*time.Millisecond))
	got := r.outputTiming()
	if got.FirstBatchBytes != 5 || got.TotalBytes != 18 || got.LargestBatchBytes != 9 || got.MaxInterarrivalNS != int64(300*time.Millisecond) || got.ArrivalCount != 3 {
		t.Fatalf("batch statistics = %+v", got)
	}
}

func TestOutputTimingProtocolDeltas(t *testing.T) {
	for _, tc := range []struct{ protocol, payload, want string }{
		{"chat", `{"choices":[{"delta":{"reasoning_content":"think","content":"answer","tool_calls":[{"function":{"arguments":"{}"}}]}}]}`, "answerthink{}"},
		{"claude", `{"type":"content_block_delta","delta":{"thinking":"hidden on client"}}`, "hidden on client"},
		{"gemini", `{"candidates":[{"content":{"parts":[{"text":"thought","thought":true},{"functionCall":{"name":"tool","args":{"x":1}}}]}}]}`, `thoughttool{"x":1}`},
		{"responses", `{"type":"response.function_call_arguments.delta","delta":"{}"}`, "{}"},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			got, unsupported := outputDeltaText(tc.protocol, []byte(tc.payload))
			if unsupported || got != tc.want {
				t.Fatalf("delta=%q unsupported=%v, want %q", got, unsupported, tc.want)
			}
		})
	}
}

func TestGenerationBodyInvalidReasonsPreservePayload(t *testing.T) {
	for _, tc := range []struct{ name, payload, protocol, reason string }{
		{"malformed", "data: {broken}\n\n", "chat", "malformed_stream"},
		{"line-limit", "data: " + strings.Repeat("x", generationFrameBytes+1) + "\n\n", "chat", "frame_limit"},
		{"multiline-limit", strings.Repeat("data: "+strings.Repeat("x", 1000)+"\n", 1100) + "\n", "chat", "frame_limit"},
		{"multimodal", "data: {\"type\":\"response.audio.delta\",\"delta\":\"YWJj\"}\n\n", "responses", "unsupported_protocol"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewUsageReporter(context.Background(), "test", "test", nil)
			r.StartUpstreamTiming()
			payload := []byte(tc.payload)
			observer := r.generationSSEObserver(tc.protocol)
			observer(payload, r.dispatchAt.Add(time.Second), io.EOF)
			if string(payload) != tc.payload {
				t.Fatal("observer changed source bytes")
			}
			if timing := r.outputTiming(); timing.Status != "unavailable" || timing.Reason != tc.reason {
				t.Fatalf("reason = %+v, want %s", timing, tc.reason)
			}
		})
	}
}
