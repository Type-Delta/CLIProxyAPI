package model

import (
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

func estimatorEvent(id int, at time.Time, rate int64) Event {
	credential, algorithm := "credential", "hmac-sha256"
	stream := true
	return Event{
		AttemptID: fmt.Sprintf("%032x", id), ProxyRequestID: fmt.Sprintf("%032x", id),
		KeyID: "key", Provider: "provider", Model: "model", ExecutorType: "executor",
		CredentialID: &credential, CredentialIDAlgorithm: &algorithm, RequestedAt: at,
		Succeeded: true, LatencyMS: 2000, Stream: &stream,
		Source:       "native",
		Tokens:       TokenUsage{Input: 100, Output: rate, Total: 100 + rate, Schema: AccountingSchemaNormalizedV1, Quality: TokenQualityExact},
		OutputTiming: &OutputTiming{Version: 1, Protocol: "chat", Source: "source_reader", Coverage: "visible_deltas", Status: "observed", FirstArrivalNS: 1, LastArrivalNS: int64(time.Second) + 1, ArrivalCount: 10, WindowTokens: rate, TokenQuality: TokenQualityEstimated},
	}
}

func TestEstimateArrivalValidationCannotTrainOnConcurrentRequests(t *testing.T) {
	target, history := estimatorFixture(100)
	// These validation requests start before the final ten training requests
	// complete, but finish after all fifty. A completion-only split would admit.
	for i := 50; i < 70; i++ {
		history[i].RequestedAt = target.RequestedAt.Add(time.Duration(i-110) * time.Minute)
		history[i].LatencyMS = int64(30 * time.Minute / time.Millisecond)
	}
	got := EstimateArrivalThroughput(target, history)
	if got.Status != "unavailable" || got.Reason != "insufficient_history" || got.TrainingSamples != 40 || got.ValidationSamples != 20 {
		t.Fatalf("concurrent validation=%+v", got)
	}
	// Enough strictly earlier training can still admit, without fitting on the
	// concurrent labels or refitting after validation.
	for i := 0; i < 15; i++ {
		history = append(history, estimatorEvent(200+i, target.RequestedAt.Add(time.Duration(i-130)*time.Minute), 100))
	}
	got = EstimateArrivalThroughput(target, history)
	if got.Status != "estimated" || got.TrainingSamples != 55 || *got.TokensPerSecond != 100 {
		t.Fatalf("causal validation=%+v", got)
	}
}

func TestEstimateArrivalHistoryMustBeExplicitlyNative(t *testing.T) {
	for _, source := range []string{"", "import", "future-source"} {
		target, history := estimatorFixture(100)
		for i := range history {
			history[i].Source = source
		}
		if got := EstimateArrivalThroughput(target, history); got.Reason != "insufficient_history" {
			t.Fatalf("source %q admitted: %+v", source, got)
		}
	}
}

func estimatorFixture(rate int64) (Event, []Event) {
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	target := estimatorEvent(1000, at, rate)
	target.OutputTiming.Status, target.OutputTiming.Reason = "unavailable", "insufficient_tokens"
	target.OutputTiming.WindowTokens, target.OutputTiming.TokenQuality = 0, TokenQualityMissing
	history := make([]Event, 70)
	for i := range history {
		history[i] = estimatorEvent(i+1, at.Add(time.Duration(i-100)*time.Minute), rate)
	}
	return target, history
}

func TestEstimateArrivalStableMissingQualityTargetAndUncappedRate(t *testing.T) {
	target, history := estimatorFixture(1500)
	got := EstimateArrivalThroughput(target, history)
	if got.Status != "estimated" || got.Method != "cohort_median_v1" || got.TokensPerSecond == nil || *got.TokensPerSecond != 1500 || got.HistoricalP10 == nil || *got.HistoricalP10 != 1500 || *got.HistoricalP90 != 1500 || got.TrainingSamples != 50 || got.ValidationSamples != 20 || *got.ValidationMedianAPE != 0 || *got.ValidationRangeCoverage != 1 || *got.RelativeSpread != 0 || !got.AsOf.Equal(target.RequestedAt) {
		t.Fatalf("estimate=%+v", got)
	}
}

func TestEstimateArrivalRejectsSparseDriftAndStaleHistory(t *testing.T) {
	for _, name := range []string{"sparse", "drift", "stale", "future_completion", "same_request", "duplicate_requests"} {
		t.Run(name, func(t *testing.T) {
			target, history := estimatorFixture(100)
			want := "insufficient_history"
			switch name {
			case "sparse":
				history = history[:69]
			case "drift":
				want = "validation_failed"
				for i := 50; i < 70; i++ {
					history[i].OutputTiming.WindowTokens = 300
				}
			case "stale":
				want = "stale_history"
				for i := range history {
					history[i].RequestedAt = history[i].RequestedAt.Add(-3 * time.Hour)
				}
			case "future_completion":
				history[0].LatencyMS = int64(101 * time.Minute / time.Millisecond)
			case "same_request":
				history[0].ProxyRequestID = target.ProxyRequestID
			case "duplicate_requests":
				history[0].ProxyRequestID = history[1].ProxyRequestID
			}
			got := EstimateArrivalThroughput(target, history)
			if got.Status != "unavailable" || got.Reason != want || got.TokensPerSecond != nil {
				t.Fatalf("estimate=%+v, want %s", got, want)
			}
		})
	}
}

func TestEstimateArrivalCohortIsolation(t *testing.T) {
	changes := map[string]func(*Event){
		"key":            func(e *Event) { e.KeyID = "other" },
		"provider":       func(e *Event) { e.Provider = "other" },
		"model":          func(e *Event) { e.Model = "other" },
		"executor":       func(e *Event) { e.ExecutorType = "other" },
		"credential":     func(e *Event) { v := "other"; e.CredentialID = &v },
		"algorithm":      func(e *Event) { v := "other"; e.CredentialIDAlgorithm = &v },
		"protocol":       func(e *Event) { e.OutputTiming.Protocol = "responses" },
		"source":         func(e *Event) { e.OutputTiming.Source = "websocket_reader" },
		"coverage":       func(e *Event) { e.OutputTiming.Coverage = "wire_deltas" },
		"stream":         func(e *Event) { v := false; e.Stream = &v },
		"hint":           func(e *Event) { v := "high"; e.ReasoningEffort = &v },
		"requested_tier": func(e *Event) { v := "default"; e.ServiceTierRequested = &v },
		"used_tier":      func(e *Event) { v := "default"; e.ServiceTierUsed = &v },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			target, history := estimatorFixture(100)
			for i := range history {
				change(&history[i])
			}
			if got := EstimateArrivalThroughput(target, history); got.Reason != "insufficient_history" {
				t.Fatalf("estimate=%+v", got)
			}
		})
	}
}

func TestEstimateArrivalHistoryQualification(t *testing.T) {
	changes := map[string]func(*Event){
		"failed":           func(e *Event) { e.Succeeded = false },
		"imported":         func(e *Event) { e.ImportBatchID = "import" },
		"short":            func(e *Event) { e.OutputTiming.LastArrivalNS = int64(time.Second) },
		"few_tokens":       func(e *Event) { e.OutputTiming.WindowTokens = 63 },
		"few_reads":        func(e *Event) { e.OutputTiming.ArrivalCount = 9 },
		"missing_quality":  func(e *Event) { e.OutputTiming.TokenQuality = TokenQualityMissing },
		"bad_accounting":   func(e *Event) { e.Tokens.Total = 1 },
		"outside_window":   func(e *Event) { e.RequestedAt = e.RequestedAt.Add(-24 * time.Hour) },
		"responded_late":   func(e *Event) { v := e.RequestedAt.Add(3 * time.Hour); e.RespondedAt = &v },
		"completion_equal": func(e *Event) { e.LatencyMS = int64(100 * time.Minute / time.Millisecond) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			target, history := estimatorFixture(100)
			change(&history[0])
			if got := EstimateArrivalThroughput(target, history); got.Reason != "insufficient_history" {
				t.Fatalf("estimate=%+v", got)
			}
		})
	}
}

func TestEstimateArrivalTargetEligibilityAndObservedPreservation(t *testing.T) {
	target, history := estimatorFixture(100)
	for _, change := range []func(*Event){
		func(e *Event) { e.OutputTiming = nil },
		func(e *Event) { e.Stream = nil },
		func(e *Event) { e.CredentialID = nil },
		func(e *Event) { e.Provider = "unknown" },
		func(e *Event) { e.Tokens.Schema = AccountingSchemaRawFallbackV1 },
		func(e *Event) { e.Tokens.Output = 0 },
		func(e *Event) { e.OutputTiming.Reason = "malformed_stream" },
	} {
		e, _ := estimatorFixture(100)
		change(&e)
		if got := EstimateArrivalThroughput(e, history); got.Reason != "ineligible_metadata" {
			t.Fatalf("estimate=%+v", got)
		}
	}
	target.Succeeded = false
	if got := EstimateArrivalThroughput(target, history); got.Reason != "failed_request" {
		t.Fatalf("estimate=%+v", got)
	}
	target = estimatorEvent(1000, target.RequestedAt, 100)
	target.EnrichThroughput()
	before := target
	if got := EstimateArrivalThroughput(target, history); got.Reason != "observed_available" {
		t.Fatalf("estimate=%+v", got)
	}
	if got := EvaluateArrivalThroughputEstimate(target, history); got.Status != "estimated" {
		t.Fatalf("evaluation=%+v", got)
	}
	if !reflect.DeepEqual(before, target) {
		t.Fatal("predictor modified observation")
	}
}

func TestEstimateArrivalFrozenTrainingAndCompletionOrdering(t *testing.T) {
	target, history := estimatorFixture(100)
	// Training has a wide range, validation is consistently 110. Refit would move the median.
	for i := range history {
		if i < 25 {
			history[i].OutputTiming.WindowTokens = 90
		} else {
			history[i].OutputTiming.WindowTokens = 110
		}
	}
	// Reversing input must not change completion ordering or the frozen median of 100.
	for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
		history[i], history[j] = history[j], history[i]
	}
	got := EstimateArrivalThroughput(target, history)
	if got.Status != "estimated" || *got.TokensPerSecond != 100 {
		t.Fatalf("estimate=%+v", got)
	}
	// Only the latest validation completion needs to be fresh.
	for i := range history {
		history[i].RequestedAt = history[i].RequestedAt.Add(-3 * time.Hour)
	}
	history[0].RequestedAt = target.RequestedAt.Add(-time.Minute)
	if got = EstimateArrivalThroughput(target, history); got.Status != "estimated" {
		t.Fatalf("fresh latest=%+v", got)
	}
}

func TestEstimateArrivalTailErrorAndRangeCoverageGates(t *testing.T) {
	for _, name := range []string{"tail_error", "range_coverage"} {
		t.Run(name, func(t *testing.T) {
			target, history := estimatorFixture(100)
			if name == "tail_error" {
				for i := 67; i < 70; i++ {
					history[i].OutputTiming.WindowTokens = 300
				}
			} else {
				for i := 50; i < 70; i++ {
					history[i].OutputTiming.WindowTokens = 110
				}
			}
			got := EstimateArrivalThroughput(target, history)
			if got.Reason != "validation_failed" || got.ValidationMedianAPE == nil || *got.ValidationMedianAPE > .25 || got.TokensPerSecond != nil {
				t.Fatalf("estimate=%+v", got)
			}
			if name == "tail_error" && (*got.ValidationP90APE <= .6 || *got.ValidationRangeCoverage < .75) {
				t.Fatalf("tail gate evidence=%+v", got)
			}
			if name == "range_coverage" && (*got.ValidationP90APE > .6 || *got.ValidationRangeCoverage != 0) {
				t.Fatalf("coverage gate evidence=%+v", got)
			}
		})
	}
}

func TestEstimateArrivalLatest200BudgetAndMixedCounterQuality(t *testing.T) {
	target, _ := estimatorFixture(1500)
	history := make([]Event, 300)
	for i := range history {
		rate := int64(1500)
		if i < 100 {
			rate = 10000
		}
		history[i] = estimatorEvent(i+1, target.RequestedAt.Add(time.Duration(i-400)*time.Minute), rate)
		if i%2 == 0 {
			history[i].OutputTiming.TokenQuality = TokenQualityExact
		}
	}
	got := EstimateArrivalThroughput(target, history)
	if got.Status != "estimated" || got.TrainingSamples != 180 || got.ValidationSamples != 20 || *got.TokensPerSecond != 1500 {
		t.Fatalf("bounded estimate=%+v", got)
	}
}

func TestArrivalEstimateCompletionIsConservativeAndOverflowSafe(t *testing.T) {
	e, _ := estimatorFixture(100)
	base := e.RequestedAt.Add(time.Duration(e.LatencyMS) * time.Millisecond)
	for _, at := range []time.Time{e.RequestedAt.Add(-time.Second), base.Add(time.Minute)} {
		e.RespondedAt = &at
		got, ok := ArrivalEstimateCompletion(e)
		want := base
		if at.After(want) {
			want = at
		}
		if !ok || !got.Equal(want) {
			t.Fatalf("completion=%v ok=%v want=%v", got, ok, want)
		}
	}
	for _, latency := range []int64{0, -1, math.MaxInt64, math.MaxInt64 / int64(time.Millisecond)} {
		e.LatencyMS = latency
		if _, ok := ArrivalEstimateCompletion(e); ok {
			t.Fatalf("accepted duration %d", latency)
		}
	}
}
