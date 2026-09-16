package model

import (
	"math"
	"testing"
)

func throughputMS(value int64) *int64 { return &value }

func throughputRatio(value float64) *float64 { return &value }

func throughputEvent(ackMS, firstTokenMS, generationMS *int64, output int64) Event {
	return Event{
		ProviderLatencyMS:   ackMS,
		FirstTokenLatencyMS: firstTokenMS,
		GenerationTimeMS:    generationMS,
		Tokens:              TokenUsage{Output: output},
	}
}

func assertThroughput(t *testing.T, event Event, median *float64, want float64, wantEstimated, wantOK bool) {
	t.Helper()
	value, estimated, ok := event.Throughput(median)
	if ok != wantOK || estimated != wantEstimated || (ok && math.Abs(value-want) > 1e-6) {
		t.Fatalf("Throughput() = %v estimated=%v ok=%v, want %v estimated=%v ok=%v",
			value, estimated, ok, want, wantEstimated, wantOK)
	}
}

func TestThroughputKeepsPlausibleObservations(t *testing.T) {
	event := throughputEvent(throughputMS(100), throughputMS(300), throughputMS(1_500), 150)
	assertThroughput(t, event, throughputRatio(100), 100, false, true)
}

func TestThroughputEstimatesCompressedGeneration(t *testing.T) {
	// 10ms of generation against 300ms of pre-generation wait is impossible, so the rate comes
	// from the full span minus the credential's median acknowledgement instead.
	event := throughputEvent(throughputMS(100), throughputMS(300), throughputMS(10), 100)
	assertThroughput(t, event, throughputRatio(110), 500, true, true)
}

func TestThroughputThresholdBoundaryKeepsObservation(t *testing.T) {
	// (100+200)*0.08 = 24 is exactly the recorded generation, so the comparison stays observed.
	event := throughputEvent(throughputMS(100), throughputMS(300), throughputMS(24), 240)
	assertThroughput(t, event, throughputRatio(100), 10_000, false, true)
}

func TestThroughputTreatsMissingWaitAndGenerationAsZero(t *testing.T) {
	event := throughputEvent(throughputMS(1_000), nil, nil, 100)
	assertThroughput(t, event, throughputRatio(800), 500, true, true)
}

func TestThroughputRequiresUsableAcknowledgement(t *testing.T) {
	// Without an acknowledgement the whole first-token latency is wait, so a compressed
	// generation still triggers the estimate and then fails for the missing acknowledgement.
	for name, event := range map[string]Event{
		"missing": throughputEvent(nil, throughputMS(300), throughputMS(10), 100),
		"zero":    throughputEvent(throughputMS(0), throughputMS(300), throughputMS(10), 100),
	} {
		if _, _, ok := event.Throughput(throughputRatio(100)); ok {
			t.Fatalf("%s acknowledgement published a rate", name)
		}
	}
}

func TestThroughputWithoutAcknowledgementKeepsPlausibleObservation(t *testing.T) {
	// The estimate needs an acknowledgement, but a plausible recorded generation does not.
	event := throughputEvent(nil, throughputMS(300), throughputMS(1_500), 150)
	assertThroughput(t, event, throughputRatio(100), 100, false, true)
}

func TestThroughputRequiresUsableMedian(t *testing.T) {
	event := throughputEvent(throughputMS(100), throughputMS(300), throughputMS(10), 100)
	for name, median := range map[string]*float64{
		"missing":  nil,
		"zero":     throughputRatio(0),
		"negative": throughputRatio(-1),
	} {
		if _, _, ok := event.Throughput(median); ok {
			t.Fatalf("%s median published a rate", name)
		}
	}
}

func TestThroughputRejectsNonPositiveDuration(t *testing.T) {
	event := throughputEvent(throughputMS(100), throughputMS(300), throughputMS(0), 100)
	for _, median := range []float64{300, 400, 500} {
		if _, _, ok := event.Throughput(throughputRatio(median)); ok {
			t.Fatalf("median %v published a non-positive duration", median)
		}
	}
}

func TestThroughputRejectsMissingOutput(t *testing.T) {
	event := throughputEvent(throughputMS(100), throughputMS(300), throughputMS(1_500), 0)
	assertThroughput(t, event, throughputRatio(100), 0, false, false)
}

func TestThroughputWithoutTimingInputsIsUnavailable(t *testing.T) {
	event := throughputEvent(nil, nil, nil, 100)
	assertThroughput(t, event, throughputRatio(100), 0, false, false)
}
