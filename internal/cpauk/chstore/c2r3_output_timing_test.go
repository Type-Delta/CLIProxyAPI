package chstore

import (
	"context"

	"encoding/json"

	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestR3OutputTimingLegacyAndImportBackupRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.db")
	config := Config{Path: path, MaxStorageBytes: 64 << 20}
	database, err := openR3Store(t, config)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	legacy := throughputStoreEvent("1", strings.Repeat("5", 64), end.Add(-time.Minute), 200, 500, 1000, 480)
	if err := database.WriteBatch(ctx, []model.Event{legacy}); err != nil {
		t.Fatal(err)
	}
	query := r3V2Query(model.OperationEvents, end.Add(-time.Hour), end)
	found, ok, err := database.EventByAttemptID(ctx, legacy.AttemptID, query)
	if err != nil || !ok || found.OutputTiming != nil || found.Stream != nil || found.ReasoningEffort != nil || found.GenerationTimeMS == nil || *found.GenerationTimeMS != 1000 || found.TokensPerSecond != nil || found.ThroughputInfo == nil || found.ThroughputInfo.Reason != "legacy_timing" {
		t.Fatalf("legacy after migration=%+v ok=%v err=%v", found, ok, err)
	}
	event := throughputStoreEvent("2", strings.Repeat("5", 64), end.Add(-2*time.Minute), 200, 500, 1000, 480)
	event.Stream = new(false)
	effort := "high"
	event.ReasoningEffort = &effort
	event.OutputTiming = &model.OutputTiming{FirstBatchBytes: 10, TotalBytes: 100, LargestBatchBytes: 50, MaxInterarrivalNS: 700000000, Version: 1, Protocol: "claude", Source: "source_reader", Status: "observed", FirstArrivalNS: 1000, LastArrivalNS: 1000001000, ArrivalCount: 4, WindowTokens: 1500, TokenQuality: model.TokenQualityEstimated, Coverage: "visible_deltas"}
	stale := 999999.0
	event.TokensPerSecond = &stale
	event.EffectiveOutputTokensPerSecond = &stale
	event.ThroughputInfo = &model.ThroughputInfo{Status: "observed", Method: "stale-prediction", TokensPerSecond: &stale}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var imported model.Event
	if err := json.Unmarshal(encoded, &imported); err != nil {
		t.Fatal(err)
	}
	if count, err := database.WriteImportBatch(ctx, []model.Event{imported}, "timing-roundtrip"); err != nil || count != 1 {
		t.Fatalf("import count=%d err=%v", count, err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if _, err := database.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if err := database.Restore(ctx, backup, backup+".manifest.json"); err != nil {
		t.Fatal(err)
	}
	found, ok, err = database.EventByAttemptID(ctx, event.AttemptID, query)
	if err != nil || !ok || !reflect.DeepEqual(found.OutputTiming, event.OutputTiming) || found.Stream == nil || *found.Stream || found.ReasoningEffort == nil || *found.ReasoningEffort != "high" || found.Source != "import" || found.ImportBatchID != "timing-roundtrip" || found.TokensPerSecond == nil || *found.TokensPerSecond != 1500 || found.ThroughputInfo == nil || found.ThroughputInfo.Method != "arrival_window_v1" || found.EffectiveOutputTokensPerSecond != nil {
		t.Fatalf("import/backup roundtrip=%+v ok=%v err=%v", found, ok, err)
	}
}
