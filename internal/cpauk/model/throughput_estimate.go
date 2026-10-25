package model

import (
	"cmp"
	"math"
	"slices"
	"time"
)

// ThroughputEstimate predicts comparable local arrival rates, independently of
// observed throughput. Historical quantiles are not confidence probabilities.
type ThroughputEstimate struct {
	Status                  string   `json:"status"`
	Method                  string   `json:"method"`
	TokensPerSecond         *float64 `json:"tokens_per_second,omitempty"`
	HistoricalP10           *float64 `json:"historical_p10,omitempty"`
	HistoricalP90           *float64 `json:"historical_p90,omitempty"`
	TrainingSamples         int      `json:"training_samples"`
	ValidationSamples       int      `json:"validation_samples"`
	ValidationMedianAPE     *float64 `json:"validation_median_ape,omitempty"`
	ValidationP90APE        *float64 `json:"validation_p90_ape,omitempty"`
	ValidationRangeCoverage *float64 `json:"validation_range_coverage,omitempty"`
	RelativeSpread          *float64 `json:"relative_spread,omitempty"`
	// History bounds are completion times of the labels actually used.
	HistoryStart *time.Time `json:"history_start,omitempty"`
	HistoryEnd   *time.Time `json:"history_end,omitempty"`
	AsOf         time.Time  `json:"as_of"`
	Reason       string     `json:"reason,omitempty"`
}

// EstimateArrivalThroughput uses only earlier completed observations. Callers
// omit the result for observed targets and preserve their existing rate aliases.
func EstimateArrivalThroughput(target Event, history []Event) ThroughputEstimate {
	return estimateArrivalThroughput(target, history, false)
}

// EvaluateArrivalThroughputEstimate evaluates the production predictor on an
// observed target without using its measured rate or modifying either input.
func EvaluateArrivalThroughputEstimate(target Event, history []Event) ThroughputEstimate {
	return estimateArrivalThroughput(target, history, true)
}

func estimateArrivalThroughput(target Event, history []Event, evaluation bool) ThroughputEstimate {
	result := ThroughputEstimate{Status: "unavailable", Method: "cohort_median_v1", AsOf: target.RequestedAt, Reason: "ineligible_metadata"}
	if !evaluation && target.ArrivalThroughput().TokensPerSecond != nil {
		result.Reason = "observed_available"
		return result
	}
	if !target.Succeeded {
		result.Reason = "failed_request"
		return result
	}
	if !arrivalEstimateMetadata(target) {
		return result
	}
	type observation struct {
		requestID, attemptID   string
		requestedAt, completed time.Time
		rate                   float64
	}
	observations := make([]observation, 0, min(len(history), 200))
	start := target.RequestedAt.Add(-24 * time.Hour)
	for _, event := range history {
		if event.ProxyRequestID == target.ProxyRequestID || event.RequestedAt.Before(start) {
			continue
		}
		rate, completed, ok := event.ArrivalEstimateObservation()
		if !ok || !completed.Before(target.RequestedAt) || !sameArrivalCohort(target, event) {
			continue
		}
		observations = append(observations, observation{event.ProxyRequestID, event.AttemptID, event.RequestedAt, completed, rate})
	}
	// Latest completion wins for repeated requests. Stable identities resolve ties.
	slices.SortFunc(observations, func(a, b observation) int {
		if order := b.completed.Compare(a.completed); order != 0 {
			return order
		}
		if order := cmp.Compare(a.requestID, b.requestID); order != 0 {
			return order
		}
		return cmp.Compare(a.attemptID, b.attemptID)
	})
	seen := make(map[string]struct{}, 200)
	unique := observations[:0]
	for _, observation := range observations {
		if _, exists := seen[observation.requestID]; exists {
			continue
		}
		seen[observation.requestID] = struct{}{}
		unique = append(unique, observation)
		if len(unique) == 200 {
			break
		}
	}
	result.ValidationSamples = min(len(unique), 20)
	validation := unique[:result.ValidationSamples]
	training := unique[result.ValidationSamples:result.ValidationSamples]
	if len(validation) > 0 {
		validationStart := validation[0].requestedAt
		for _, observation := range validation[1:] {
			if observation.requestedAt.Before(validationStart) {
				validationStart = observation.requestedAt
			}
		}
		// Validation must model a predictor that could have existed before
		// every held-out request started, including overlapping long requests.
		for _, observation := range unique[result.ValidationSamples:] {
			if observation.completed.Before(validationStart) {
				training = append(training, observation)
			}
		}
	}
	result.TrainingSamples = len(training)
	result.Reason = "insufficient_history"
	if len(unique) > 0 {
		first, last := validation[len(validation)-1].completed, validation[0].completed
		if len(training) > 0 {
			first = training[len(training)-1].completed
		}
		result.HistoryStart, result.HistoryEnd = &first, &last
	}
	if result.TrainingSamples < 50 {
		return result
	}
	if target.RequestedAt.Sub(unique[0].completed) > 2*time.Hour {
		result.Reason = "stale_history"
		return result
	}
	trainingRates := make([]float64, 0, result.TrainingSamples)
	for _, observation := range training {
		trainingRates = append(trainingRates, observation.rate)
	}
	slices.Sort(trainingRates)
	median, p10, p90 := arrivalQuantile(trainingRates, .5), arrivalQuantile(trainingRates, .1), arrivalQuantile(trainingRates, .9)
	errors := make([]float64, 0, 20)
	covered := 0
	for _, observation := range validation {
		errors = append(errors, math.Abs(median-observation.rate)/observation.rate)
		if observation.rate >= p10 && observation.rate <= p90 {
			covered++
		}
	}
	slices.Sort(errors)
	medianAPE, p90APE, coverage := arrivalQuantile(errors, .5), arrivalQuantile(errors, .9), float64(covered)/20
	spread := (p90 - p10) / median
	result.ValidationMedianAPE, result.ValidationP90APE = &medianAPE, &p90APE
	result.ValidationRangeCoverage, result.RelativeSpread = &coverage, &spread
	if medianAPE > .25 || p90APE > .60 || coverage < .75 {
		result.Reason = "validation_failed"
		return result
	}
	result.Status, result.Reason = "estimated", ""
	result.TokensPerSecond, result.HistoricalP10, result.HistoricalP90 = &median, &p10, &p90
	return result
}

// ArrivalEstimateObservation qualifies a raw training label. Store readers use
// the same checks while streaming indexed candidates, before bounding history.
func (event Event) ArrivalEstimateObservation() (float64, time.Time, bool) {
	if !arrivalEstimateMetadata(event) || !event.Succeeded || event.ImportBatchID != "" || event.Source != "native" || event.ProxyRequestID == "" {
		return 0, time.Time{}, false
	}
	timing := event.OutputTiming
	if timing.Status != "observed" || timing.LastArrivalNS-timing.FirstArrivalNS < int64(time.Second) || timing.WindowTokens < 64 || timing.ArrivalCount < 10 {
		return 0, time.Time{}, false
	}
	rate := event.ArrivalThroughput().TokensPerSecond
	completed, ok := ArrivalEstimateCompletion(event)
	if rate == nil || !ok {
		return 0, time.Time{}, false
	}
	return *rate, completed, true
}

// ArrivalEstimateCompletion conservatively combines attempt duration with any
// later client response. Invalid or overflowing timestamps cannot enter history.
func ArrivalEstimateCompletion(event Event) (time.Time, bool) {
	if event.RequestedAt.IsZero() || event.LatencyMS <= 0 || event.LatencyMS > math.MaxInt64/int64(time.Millisecond) {
		return time.Time{}, false
	}
	start, duration := event.RequestedAt.UnixNano(), event.LatencyMS*int64(time.Millisecond)
	if !time.Unix(0, start).Equal(event.RequestedAt) || start > math.MaxInt64-duration {
		return time.Time{}, false
	}
	completed := time.Unix(0, start+duration).UTC()
	if event.RespondedAt != nil && event.RespondedAt.After(completed) {
		completed = *event.RespondedAt
	}
	return completed, true
}

func arrivalEstimateMetadata(event Event) bool {
	timing, tokens := event.OutputTiming, event.Tokens
	if oneOf(event.Provider, "", "unknown", "Unknown") || oneOf(event.Model, "", "unknown", "Unknown") || oneOf(event.ExecutorType, "", "unknown", "Unknown") || event.KeyID == "" || event.CredentialID == nil || *event.CredentialID == "" || event.CredentialIDAlgorithm == nil || *event.CredentialIDAlgorithm == "" || event.Stream == nil || event.RequestedAt.IsZero() {
		return false
	}
	if timing == nil || timing.Validate() != nil || timing.Protocol == "" || timing.Source == "" || timing.Coverage == "" || !oneOf(timing.Reason, "", "insufficient_arrivals", "insufficient_span", "insufficient_tokens", "backpressure", "coverage_limit") {
		return false
	}
	if event.ReasoningEffort != nil && !ValidReasoningEffort(*event.ReasoningEffort) {
		return false
	}
	return tokens.Schema == AccountingSchemaNormalizedV1 && (tokens.Quality == TokenQualityExact || tokens.Quality == TokenQualityEstimated) && tokens.Output > 0 && tokens.Input >= 0 && tokens.Input <= tokens.Total && tokens.Output <= tokens.Total-tokens.Input && tokens.Reasoning >= 0 && tokens.Reasoning <= tokens.Output
}

func sameArrivalCohort(a, b Event) bool {
	return a.KeyID == b.KeyID && a.Provider == b.Provider && a.Model == b.Model && a.ExecutorType == b.ExecutorType && sameArrivalOptional(a.CredentialID, b.CredentialID) && sameArrivalOptional(a.CredentialIDAlgorithm, b.CredentialIDAlgorithm) &&
		a.OutputTiming.Version == b.OutputTiming.Version && a.OutputTiming.Protocol == b.OutputTiming.Protocol && a.OutputTiming.Source == b.OutputTiming.Source && a.OutputTiming.Coverage == b.OutputTiming.Coverage && sameArrivalOptional(a.Stream, b.Stream) &&
		sameArrivalOptional(a.ReasoningEffort, b.ReasoningEffort) && sameArrivalOptional(a.ServiceTierRequested, b.ServiceTierRequested) && sameArrivalOptional(a.ServiceTierUsed, b.ServiceTierUsed)
}

func sameArrivalOptional[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// arrivalQuantile linearly interpolates sorted empirical samples.
func arrivalQuantile(sorted []float64, fraction float64) float64 {
	position := float64(len(sorted)-1) * fraction
	lower := int(position)
	upper := min(lower+1, len(sorted)-1)
	return sorted[lower] + (sorted[upper]-sorted[lower])*(position-float64(lower))
}
