package model

import (
	"fmt"
	"time"
)

// OutputTiming retains source-reader evidence without storing generated content.
type OutputTiming struct {
	Version           int          `json:"version"`
	Protocol          string       `json:"protocol"`
	Source            string       `json:"source"`
	Status            string       `json:"status"`
	Reason            string       `json:"reason,omitempty"`
	FirstArrivalNS    int64        `json:"first_arrival_ns"`
	LastArrivalNS     int64        `json:"last_arrival_ns"`
	ArrivalCount      int64        `json:"arrival_count"`
	WindowTokens      int64        `json:"window_tokens"`
	TokenQuality      TokenQuality `json:"token_quality"`
	Coverage          string       `json:"coverage"`
	FirstBatchBytes   int64        `json:"first_batch_bytes"`
	TotalBytes        int64        `json:"total_bytes"`
	LargestBatchBytes int64        `json:"largest_batch_bytes"`
	MaxInterarrivalNS int64        `json:"max_interarrival_ns"`
}

// ThroughputInfo describes local arrival throughput independently of billing usage.
type ThroughputInfo struct {
	Status          string       `json:"status"`
	Method          string       `json:"method"`
	TokensPerSecond *float64     `json:"tokens_per_second,omitempty"`
	Basis           string       `json:"basis,omitempty"`
	TokenQuality    TokenQuality `json:"token_quality,omitempty"`
	Reason          string       `json:"reason,omitempty"`
}

func (t *OutputTiming) Validate() error {
	if t == nil {
		return nil
	}
	if t.FirstBatchBytes < 0 || t.TotalBytes < 0 || t.LargestBatchBytes < 0 || t.MaxInterarrivalNS < 0 || t.FirstBatchBytes > t.LargestBatchBytes || t.LargestBatchBytes > t.TotalBytes || t.LastArrivalNS >= t.FirstArrivalNS && t.MaxInterarrivalNS > t.LastArrivalNS-t.FirstArrivalNS {
		return fmt.Errorf("invalid output timing batch measurements")
	}
	// Unsupported paths still report why timing is absent, without inventing protocol evidence.
	unsupported := t.Status == "unavailable" && t.Reason == "unsupported_protocol" && t.TokenQuality == TokenQualityMissing && t.FirstArrivalNS == 0 && t.LastArrivalNS == 0 && t.ArrivalCount == 0 && t.WindowTokens == 0
	if unsupported && t.Version == 1 && oneOf(t.Protocol, "", "chat", "claude", "responses", "gemini", "kiro") && oneOf(t.Source, "", "source_reader", "websocket_reader") && oneOf(t.Coverage, "", "visible_deltas", "wire_deltas") {
		return nil
	}
	if t.Version != 1 || !oneOf(t.Protocol, "chat", "claude", "responses", "gemini", "kiro") || !oneOf(t.Source, "source_reader", "websocket_reader") || !oneOf(t.Status, "observed", "unavailable") || !t.TokenQuality.Valid() || !oneOf(t.Coverage, "visible_deltas", "wire_deltas") {
		return fmt.Errorf("invalid output timing provenance")
	}
	if t.FirstArrivalNS < 0 || t.LastArrivalNS < t.FirstArrivalNS || t.ArrivalCount < 0 || t.WindowTokens < 0 {
		return fmt.Errorf("invalid output timing measurements")
	}
	if !oneOf(t.Reason, "", "insufficient_arrivals", "insufficient_span", "insufficient_tokens", "backpressure", "frame_limit", "coverage_limit", "malformed_stream", "unsupported_protocol") || t.Status == "observed" && t.Reason != "" || t.Status == "unavailable" && t.Reason == "" {
		return fmt.Errorf("invalid output timing status reason")
	}
	return nil
}

func oneOf(value string, values ...string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

// ArrivalThroughput qualifies a matching token window using sampling evidence.
// Local receipt timing cannot establish a provider's internal execution timeline.
func (e Event) ArrivalThroughput() ThroughputInfo {
	result := ThroughputInfo{Status: "unavailable", Method: "arrival_window_v1", Reason: "legacy_timing"}
	timing := e.OutputTiming
	if timing == nil {
		return result
	}
	result.Basis, result.TokenQuality = timing.Coverage, timing.TokenQuality
	result.Reason = "unsupported_protocol"
	if timing.Validate() != nil {
		return result
	}
	if timing.Status != "observed" {
		result.Reason = timing.Reason
		return result
	}
	if timing.ArrivalCount < 3 {
		result.Reason = "insufficient_arrivals"
		return result
	}
	span := timing.LastArrivalNS - timing.FirstArrivalNS
	if span < int64(100*time.Millisecond) {
		result.Reason = "insufficient_span"
		return result
	}
	if timing.WindowTokens < 16 || timing.TokenQuality == TokenQualityMissing {
		result.Reason = "insufficient_tokens"
		return result
	}
	value := float64(timing.WindowTokens) * float64(time.Second) / float64(span)
	result.Status, result.Reason, result.TokensPerSecond = "observed", "", &value
	return result
}

// EnrichThroughput derives rates from this attempt alone, independent of query filters.
func (e *Event) EnrichThroughput() {
	info := e.ArrivalThroughput()
	e.ThroughputInfo = &info
	e.TokensPerSecond = info.TokensPerSecond
	e.SpeedEstimated = info.TokensPerSecond != nil && info.TokenQuality == TokenQualityEstimated
	e.EffectiveOutputTokensPerSecond = nil
	if e.Outcome != "canceled" && e.LatencyMS > 0 && e.Tokens.Output > 0 && e.Tokens.Schema == AccountingSchemaNormalizedV1 && (e.Tokens.Quality == TokenQualityExact || e.Tokens.Quality == TokenQualityEstimated) && e.Tokens.Reasoning <= e.Tokens.Output && e.Tokens.Input <= e.Tokens.Total && e.Tokens.Output <= e.Tokens.Total-e.Tokens.Input {
		value := float64(e.Tokens.Output) * 1000 / float64(e.LatencyMS)
		e.EffectiveOutputTokensPerSecond = &value
	}
}

// ValidReasoningEffort accepts canonical labels without retaining arbitrary request content.
func ValidReasoningEffort(value string) bool {
	return oneOf(value, "none", "auto", "minimal", "low", "medium", "high", "xhigh", "max")
}
