package model

import (
	"math"
	"testing"
)

func throughputMS(value int64) *int64 { return &value }

func throughputEvent(ackMS, firstTokenMS, generationMS *int64, output int64) Event {
	return Event{
		ProviderLatencyMS:   ackMS,
		FirstTokenLatencyMS: firstTokenMS,
		GenerationTimeMS:    generationMS,
		Tokens:              TokenUsage{Output: output},
	}
}

func assertArrivalThroughput(t *testing.T, event Event, want float64, wantEstimated, wantOK bool) {
	t.Helper()
	info := event.ArrivalThroughput()
	ok := info.TokensPerSecond != nil
	estimated := ok && info.TokenQuality == TokenQualityEstimated
	if ok != wantOK || estimated != wantEstimated || ok && math.Abs(*info.TokensPerSecond-want) > 1e-6 {
		t.Fatalf("ArrivalThroughput()=%+v, want %v estimated=%v ok=%v", info, want, wantEstimated, wantOK)
	}
}

func TestThroughputLegacyMissingGenerationDoesNotInventRate(t *testing.T) {
	event := throughputEvent(throughputMS(8887), throughputMS(8887), nil, 2756)
	assertArrivalThroughput(t, event, 0, false, false)
}

func TestThroughputQualifiedArrivalIgnoresWaitAndBillingOutput(t *testing.T) {
	event := throughputEvent(throughputMS(100), throughputMS(20000), throughputMS(1000), 9000)
	event.OutputTiming = &OutputTiming{Version: 1, Protocol: "kiro", Source: "source_reader", Status: "observed", FirstArrivalNS: 20000000000, LastArrivalNS: 21000000000, ArrivalCount: 5, WindowTokens: 1500, TokenQuality: TokenQualityEstimated, Coverage: "wire_deltas"}
	for _, wait := range []*int64{nil, throughputMS(1), throughputMS(20000)} {
		event.FirstTokenLatencyMS = wait
		assertArrivalThroughput(t, event, 1500, true, true)
	}
	event.LatencyMS = 30000
	event.Tokens.Schema = AccountingSchemaNormalizedV1
	event.Tokens.Quality = TokenQualityExact
	event.Tokens.Total = 9000
	event.Tokens.Reasoning = 1000
	event.EnrichThroughput()
	if event.EffectiveOutputTokensPerSecond == nil || *event.EffectiveOutputTokensPerSecond != 300 {
		t.Fatalf("effective rate = %v", event.EffectiveOutputTokensPerSecond)
	}
}

func TestThroughputRequiresAdequateMatchingEvidence(t *testing.T) {
	base := OutputTiming{Version: 1, Protocol: "chat", Source: "source_reader", Status: "observed", FirstArrivalNS: 1000000, LastArrivalNS: 1001000000, ArrivalCount: 3, WindowTokens: 100, TokenQuality: TokenQualityEstimated, Coverage: "visible_deltas"}
	for _, test := range []struct {
		name, reason string
		modify       func(*OutputTiming)
	}{
		{"batched", "insufficient_arrivals", func(v *OutputTiming) { v.ArrivalCount = 2 }},
		{"short", "insufficient_span", func(v *OutputTiming) { v.LastArrivalNS = 5000000 }},
		{"tiny", "insufficient_tokens", func(v *OutputTiming) { v.WindowTokens = 15 }},
		{"unknown tokens", "insufficient_tokens", func(v *OutputTiming) { v.TokenQuality = TokenQualityMissing }},
		{"backpressure", "backpressure", func(v *OutputTiming) { v.Status = "unavailable"; v.Reason = "backpressure" }},
		{"hidden coverage limited", "coverage_limit", func(v *OutputTiming) { v.Status = "unavailable"; v.Reason = "coverage_limit" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			timing := base
			test.modify(&timing)
			event := Event{OutputTiming: &timing}
			got := event.ArrivalThroughput()
			if got.Status != "unavailable" || got.Reason != test.reason || got.TokensPerSecond != nil {
				t.Fatalf("throughput = %+v", got)
			}
		})
	}
	event := Event{OutputTiming: &base}
	if got := event.ArrivalThroughput(); got.TokensPerSecond == nil || *got.TokensPerSecond != 100 {
		t.Fatalf("qualified = %+v", got)
	}
}

func TestThroughputNewUnsupportedTransportRetainsReason(t *testing.T) {
	event := Event{OutputTiming: &OutputTiming{Version: 1, Status: "unavailable", Reason: "unsupported_protocol", TokenQuality: TokenQualityMissing}}
	if err := event.OutputTiming.Validate(); err != nil {
		t.Fatal(err)
	}
	got := event.ArrivalThroughput()
	if got.Reason != "unsupported_protocol" || got.Status != "unavailable" || got.TokensPerSecond != nil {
		t.Fatalf("unsupported=%+v", got)
	}
	event.OutputTiming.Protocol = "private-provider-header"
	if err := event.OutputTiming.Validate(); err == nil {
		t.Fatal("accepted unknown timing protocol")
	}
}

func TestEffectiveOutputRateWithholdsUnqualifiedAccounting(t *testing.T) {
	for _, test := range []struct {
		name   string
		tokens TokenUsage
	}{
		{"raw", TokenUsage{Output: 9, Reasoning: 94, Total: 135, Schema: AccountingSchemaRawFallbackV1, Quality: TokenQualityExact}},
		{"inconsistent reasoning", TokenUsage{Output: 9, Reasoning: 94, Total: 135, Schema: AccountingSchemaNormalizedV1, Quality: TokenQualityExact}},
		{"inconsistent total", TokenUsage{Input: 32, Output: 103, Total: 41, Schema: AccountingSchemaNormalizedV1, Quality: TokenQualityExact}},
		{"missing", TokenUsage{Output: 103, Total: 103, Schema: AccountingSchemaNormalizedV1, Quality: TokenQualityMissing}},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := Event{LatencyMS: 1000, Tokens: test.tokens}
			event.EnrichThroughput()
			if event.EffectiveOutputTokensPerSecond != nil {
				t.Fatalf("unqualified effective rate=%v", *event.EffectiveOutputTokensPerSecond)
			}
		})
	}
}

func TestOutputTimingRejectsContradictoryBatchCounters(t *testing.T) {
	base := OutputTiming{Version: 1, Protocol: "chat", Source: "source_reader", Status: "observed", TokenQuality: TokenQualityEstimated, Coverage: "visible_deltas", LastArrivalNS: 1000000000, ArrivalCount: 3, WindowTokens: 100, FirstBatchBytes: 10, TotalBytes: 100, LargestBatchBytes: 50, MaxInterarrivalNS: 700000000}
	for _, modify := range []func(*OutputTiming){func(v *OutputTiming) { v.FirstBatchBytes = -1 }, func(v *OutputTiming) { v.LargestBatchBytes = 9 }, func(v *OutputTiming) { v.TotalBytes = 49 }, func(v *OutputTiming) { v.MaxInterarrivalNS = 1000000001 }} {
		timing := base
		modify(&timing)
		if err := timing.Validate(); err == nil {
			t.Fatalf("accepted batch counters %+v", timing)
		}
	}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
}
