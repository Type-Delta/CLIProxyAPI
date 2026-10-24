package store

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
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

func TestEventsRetainTimingAndKeepRatesIndependentOfFilters(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(ctx) })
	end := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	event := throughputStoreEvent("1", strings.Repeat("5", 64), end.Add(-time.Minute), 8887, 8887, 0, 2756)
	event.LatencyMS = 26204
	event.OutputTiming = &model.OutputTiming{Version: 1, Protocol: "kiro", Source: "source_reader", Status: "observed", FirstArrivalNS: 1000000000, LastArrivalNS: 2000000000, ArrivalCount: 3, WindowTokens: 1500, TokenQuality: model.TokenQualityEstimated, Coverage: "wire_deltas"}
	legacy := throughputStoreEvent("2", strings.Repeat("5", 64), end.Add(-2*time.Minute), 8885, 8885, 0, 100)
	legacy.Succeeded = false
	if err := database.WriteBatch(ctx, []model.Event{event, legacy}); err != nil {
		t.Fatal(err)
	}
	query := v2Query(model.OperationEvents, end.Add(-time.Hour), end)
	for _, success := range []*bool{nil, new(true)} {
		query.Filters = nil
		if success != nil {
			query.Filters = map[string]json.RawMessage{"success": json.RawMessage("true")}
		}
		page, err := database.Events(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range page.Events {
			if got.AttemptID == event.AttemptID {
				if got.OutputTiming == nil || got.TokensPerSecond == nil || *got.TokensPerSecond != 1500 || !got.SpeedEstimated || got.ThroughputInfo == nil {
					t.Fatalf("list event = %+v", got)
				}
			} else if got.TokensPerSecond != nil || got.OutputTiming != nil || got.ThroughputInfo == nil || got.ThroughputInfo.Reason != "legacy_timing" {
				t.Fatalf("legacy event = %+v", got)
			}
		}
		got, ok, err := database.EventByAttemptID(ctx, event.AttemptID, query)
		if err != nil || !ok || got.TokensPerSecond == nil || math.Abs(*got.TokensPerSecond-1500) > 1e-6 {
			t.Fatalf("detail event=%+v ok=%v err=%v", got, ok, err)
		}
	}
}
