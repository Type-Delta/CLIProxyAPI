package store

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func throughputStoreEvent(digit, credential string, requestedAt time.Time, ackMS, firstTokenMS, generationMS, output int64) model.Event {
	event := v2Event(strings.Repeat(digit, 32), strings.Repeat(digit, 31)+"e", strings.Repeat("d", 64),
		requestedAt, true, nil, 0, 0, firstTokenMS, firstTokenMS+100)
	event.ProviderLatencyMS = &ackMS
	event.FirstTokenLatencyMS = &firstTokenMS
	event.GenerationTimeMS = &generationMS
	event.Tokens.Output = output
	algorithm := model.CredentialIDAlgorithm
	event.CredentialID = &credential
	event.CredentialIDAlgorithm = &algorithm
	return event
}

// TestEventsEstimateThroughputAgainstCredentialMedian checks that the estimate normalises against
// the event's own credential over the trailing day rather than a shared or unbounded baseline.
func TestEventsEstimateThroughputAgainstCredentialMedian(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })

	end := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	credentialA, credentialB := strings.Repeat("1", 64), strings.Repeat("2", 64)
	credentialC, credentialD := strings.Repeat("3", 64), strings.Repeat("4", 64)
	events := []model.Event{
		// Credential A samples plus the target's own 300ms give a 250ms median.
		throughputStoreEvent("1", credentialA, end.Add(-3*time.Hour), 100, 300, 1_500, 150),
		throughputStoreEvent("2", credentialA, end.Add(-2*time.Hour), 200, 400, 1_500, 150),
		throughputStoreEvent("3", credentialA, end.Add(-time.Hour), 400, 600, 1_500, 150),
		// Credential B acknowledges far slower, so the same compressed generation estimates lower.
		throughputStoreEvent("4", credentialB, end.Add(-2*time.Hour), 1_000, 3_000, 1_500, 150),
		throughputStoreEvent("5", credentialB, end.Add(-time.Hour), 2_000, 4_000, 1_500, 150),
		// Credential C's slow sample predates the trailing day and must not shift its median.
		throughputStoreEvent("6", credentialC, end.Add(-26*time.Hour), 10_000, 20_000, 1_500, 150),
		throughputStoreEvent("7", credentialC, end.Add(-time.Hour), 100, 300, 1_500, 150),
		// Targets: an impossible generation interval, so the whole measured span is used instead.
		throughputStoreEvent("8", credentialA, end.Add(-30*time.Minute), 300, 600, 10, 360),
		throughputStoreEvent("9", credentialB, end.Add(-30*time.Minute), 1_500, 2_000, 10, 255),
		throughputStoreEvent("a", credentialC, end.Add(-30*time.Minute), 400, 800, 10, 280),
		// Credential D has a plausible generation, so its recorded interval stays authoritative.
		throughputStoreEvent("b", credentialD, end.Add(-30*time.Minute), 100, 300, 1_000, 100),
	}
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}

	page, err := database.Events(ctx, v2Query(model.OperationEvents, end.Add(-48*time.Hour), end))
	if err != nil {
		t.Fatal(err)
	}
	byAttemptID := map[string]model.Event{}
	for _, event := range page.Events {
		byAttemptID[event.AttemptID] = event
	}
	for attemptDigit, want := range map[string]struct {
		value     float64
		estimated bool
	}{
		"8": {1_000, true},
		"9": {500, true},
		"a": {500, true},
		"b": {100, false},
	} {
		event, ok := byAttemptID[strings.Repeat(attemptDigit, 32)]
		if !ok {
			t.Fatalf("event %q missing from the page", attemptDigit)
		}
		if event.TokensPerSecond == nil {
			t.Fatalf("event %q has no throughput", attemptDigit)
		}
		if math.Abs(*event.TokensPerSecond-want.value) > 1e-6 || event.SpeedEstimated != want.estimated {
			t.Fatalf("event %q throughput = %v estimated=%v, want %v/%v",
				attemptDigit, *event.TokensPerSecond, event.SpeedEstimated, want.value, want.estimated)
		}
	}
}

func TestEventByAttemptIDCarriesThroughput(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })

	end := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	event := throughputStoreEvent("1", strings.Repeat("5", 64), end.Add(-time.Minute), 200, 500, 20, 480)
	if err := database.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	query := v2Query(model.OperationEvents, end.Add(-time.Hour), end)
	found, ok, err := database.EventByAttemptID(ctx, event.AttemptID, query)
	if err != nil || !ok {
		t.Fatalf("EventByAttemptID() ok=%v err=%v", ok, err)
	}
	// 200ms acknowledgement plus 300ms wait plus 20ms generation minus the 200ms median.
	if found.TokensPerSecond == nil || !found.SpeedEstimated || math.Abs(*found.TokensPerSecond-1_500) > 1e-6 {
		t.Fatalf("detail throughput = %v estimated=%v", found.TokensPerSecond, found.SpeedEstimated)
	}
}
