package helps

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

const outputObservationBytes = 64 * 1024

type outputObservation struct {
	protocol, source, coverage, reason string
	content                            []byte
	firstPrefixBytes                   int
	firstBatchBytes, totalBytes        int64
	batchBytes, largestBatchBytes      int64
	maxInterarrivalNS                  int64
	arrivals                           int64
	first, last                        time.Time
}

// SetOutputTimingSource declares where output is observed, before delivery queues.
func (r *UsageReporter) SetOutputTimingSource(protocol, source, coverage string) {
	if r == nil {
		return
	}
	r.ttftMu.Lock()
	r.readerOwned = true
	r.outputObservation.protocol = protocol
	r.outputObservation.source = source
	r.outputObservation.coverage = coverage
	r.ttftMu.Unlock()
}

// ObserveOutputDeltaAt collects bounded wire content for two cumulative token
// counts at publication. Multiple frames completed by one read share a timestamp.
func (r *UsageReporter) ObserveOutputDeltaAt(text string, at time.Time) {
	if r == nil || text == "" {
		return
	}
	r.ObserveGenerationTokenAt(at)
	r.ttftMu.Lock()
	defer r.ttftMu.Unlock()
	o := &r.outputObservation
	if at.IsZero() || (!o.last.IsZero() && at.Before(o.last)) {
		if o.reason == "" {
			o.reason = "malformed_stream"
		}
		return
	}
	if o.first.IsZero() {
		o.first = at
	}
	if o.last != at {
		if !o.last.IsZero() {
			o.maxInterarrivalNS = max(o.maxInterarrivalNS, int64(at.Sub(o.last)))
		}
		o.arrivals++
		o.last = at
		o.batchBytes = 0
	}
	bytes := int64(len(text))
	o.totalBytes += bytes
	o.batchBytes += bytes
	o.largestBatchBytes = max(o.largestBatchBytes, o.batchBytes)
	if at == o.first {
		o.firstBatchBytes += bytes
	}
	if o.reason != "" {
		return
	}
	if len(o.content)+len(text) > outputObservationBytes {
		o.reason = "coverage_limit"
		o.content = nil
		return
	}
	o.content = append(o.content, text...)
	if at == o.first {
		o.firstPrefixBytes = len(o.content)
	}
}

func (r *UsageReporter) outputTiming() *usage.OutputTiming {
	if r == nil {
		return nil
	}
	r.ttftMu.RLock()
	o := r.outputObservation
	dispatch := r.dispatchAt
	invalid := r.generationInvalid
	content := string(o.content)
	r.ttftMu.RUnlock()
	timing := &usage.OutputTiming{
		Version: 1, Protocol: o.protocol, Source: o.source, Status: "unavailable",
		Reason: o.reason, ArrivalCount: o.arrivals, TokenQuality: usage.TokenQualityMissing,
		Coverage: o.coverage, FirstBatchBytes: o.firstBatchBytes, TotalBytes: o.totalBytes,
		LargestBatchBytes: o.largestBatchBytes, MaxInterarrivalNS: o.maxInterarrivalNS,
	}
	if !dispatch.IsZero() && !o.first.IsZero() {
		timing.FirstArrivalNS = max(0, int64(o.first.Sub(dispatch)))
		timing.LastArrivalNS = max(0, int64(o.last.Sub(dispatch)))
	}
	if timing.Reason != "" {
		return timing
	}
	if o.protocol == "" || invalid {
		timing.Reason = "unsupported_protocol"
		return timing
	}
	if o.arrivals < 2 {
		timing.Reason = "insufficient_arrivals"
		return timing
	}
	if timing.LastArrivalNS <= timing.FirstArrivalNS {
		timing.Reason = "insufficient_span"
		return timing
	}
	encoder, err := TokenizerForModel(r.model)
	if err != nil {
		timing.Reason = "insufficient_tokens"
		return timing
	}
	firstCount, errFirst := encoder.Count(content[:o.firstPrefixBytes])
	finalCount, errFinal := encoder.Count(content)
	if errFirst != nil || errFinal != nil {
		timing.Reason = "insufficient_tokens"
		return timing
	}
	timing.TokenQuality = usage.TokenQualityEstimated
	timing.WindowTokens = max(0, int64(finalCount-firstCount))
	if timing.WindowTokens <= 0 {
		timing.Reason = "insufficient_tokens"
		return timing
	}
	timing.Status, timing.Reason = "observed", ""
	return timing
}

// outputDeltaText extracts only substantive incremental text or tool data.
// Provider usage totals, signatures, terminal snapshots, and hidden reasoning
// counters never enter the locally tokenized numerator.
func outputDeltaText(protocol string, payload []byte) (string, bool) {
	root := gjson.ParseBytes(JSONPayload(payload))
	var text strings.Builder
	add := func(value gjson.Result) { text.WriteString(value.String()) }
	switch protocol {
	case "responses":
		event := root.Get("type").String()
		switch event {
		case "response.audio.delta", "response.image_generation_call.partial_image":
			return "", true
		case "response.shell_call_command.added":
			add(root.Get("command"))
		default:
			if strings.HasSuffix(event, ".delta") && isResponsesTokenEvent(payload, false) {
				add(root.Get("delta"))
			}
		}
	case "claude":
		switch root.Get("type").String() {
		case "content_block_delta":
			for _, key := range []string{"text", "thinking", "partial_json"} {
				add(root.Get("delta." + key))
			}
		case "content_block_start":
			add(root.Get("content_block.text"))
			add(root.Get("content_block.thinking"))
		}
	case "chat":
		for _, choice := range root.Get("choices").Array() {
			delta := choice.Get("delta")
			for _, key := range []string{"content", "reasoning_content", "reasoning", "refusal"} {
				add(delta.Get(key))
			}
			for _, tool := range delta.Get("tool_calls").Array() {
				add(tool.Get("function.name"))
				add(tool.Get("function.arguments"))
				add(tool.Get("custom.input"))
			}
		}
	case "gemini":
		candidates := root.Get("candidates")
		if !candidates.Exists() {
			candidates = root.Get("response.candidates")
		}
		for _, candidate := range candidates.Array() {
			for _, part := range candidate.Get("content.parts").Array() {
				if part.Get("inlineData.data").String() != "" {
					return "", true
				}
				add(part.Get("text"))
				add(part.Get("thoughtText"))
				if thought := part.Get("thought"); thought.Type == gjson.String {
					add(thought)
				}
				add(part.Get("functionCall.name"))
				if args := part.Get("functionCall.args"); args.Exists() {
					text.WriteString(args.Raw)
				}
			}
		}
	default:
		return "", true
	}
	return text.String(), false
}
