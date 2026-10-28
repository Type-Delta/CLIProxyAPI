package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func storeEstimatorFixture(t testing.TB) (*SQLiteStore, model.Event, []model.Event) {
	t.Helper()
	ctx := context.Background()
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(ctx) })
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	makeEvent := func(id int, at time.Time) model.Event {
		e := throughputStoreEvent(fmt.Sprintf("%x", id), strings.Repeat("5", 64), at, 200, 500, 1000, 480)
		e.AttemptID, e.ProxyRequestID = fmt.Sprintf("%032x", id), fmt.Sprintf("%032x", id)
		e.Stream = new(true)
		e.Source = "native"
		e.Tokens.Total = e.Tokens.Input + e.Tokens.Output
		e.OutputTiming = &model.OutputTiming{Version: 1, Protocol: "responses", Source: "source_reader", Coverage: "visible_deltas", Status: "observed", FirstArrivalNS: 1, LastArrivalNS: int64(time.Second) + 1, WindowTokens: 1500, ArrivalCount: 10, TokenQuality: model.TokenQualityEstimated}
		return e
	}
	history := make([]model.Event, 70)
	for i := range history {
		history[i] = makeEvent(i+1, at.Add(time.Duration(i-100)*time.Minute))
	}
	target := makeEvent(1000, at)
	target.OutputTiming.Status, target.OutputTiming.Reason = "unavailable", "insufficient_tokens"
	target.OutputTiming.WindowTokens, target.OutputTiming.TokenQuality = 0, model.TokenQualityMissing
	if err := database.WriteBatch(ctx, append(history, target)); err != nil {
		t.Fatal(err)
	}
	return database, target, history
}

func TestThroughputEstimateImportsDiscardDerivedPredictionAndCannotTrain(t *testing.T) {
	database, target, _ := storeEstimatorFixture(t)
	ctx := context.Background()
	forged := 999999.0
	target.AttemptID, target.ProxyRequestID = strings.Repeat("a", 32), strings.Repeat("b", 32)
	target.ThroughputEstimate = &model.ThroughputEstimate{Status: "estimated", Method: "forged", TokensPerSecond: &forged}
	encoded, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	var imported model.Event
	if err := json.Unmarshal(encoded, &imported); err != nil {
		t.Fatal(err)
	}
	if count, err := database.WriteImportBatch(ctx, []model.Event{imported}, "estimate-import"); err != nil || count != 1 {
		t.Fatalf("import=%d err=%v", count, err)
	}
	query := v2Query(model.OperationEvents, target.RequestedAt, target.RequestedAt.Add(time.Minute))
	got, found, err := database.EventByAttemptID(ctx, target.AttemptID, query)
	if err != nil || !found || got.ThroughputEstimate == nil || got.ThroughputEstimate.Method != "cohort_median_v1" || *got.ThroughputEstimate.TokensPerSecond != 1500 {
		t.Fatalf("import retained forged estimate: %+v found=%v err=%v", got.ThroughputEstimate, found, err)
	}
	// Replacing the native pool with imported copies must remove every label.
	if _, err := database.db.ExecContext(ctx, `UPDATE events SET import_batch_id='imported-history' WHERE requested_at_ns < ?`, target.RequestedAt.UnixNano()); err != nil {
		t.Fatal(err)
	}
	got, found, err = database.EventByAttemptID(ctx, target.AttemptID, query)
	if err != nil || !found || got.ThroughputEstimate == nil || got.ThroughputEstimate.Reason != "insufficient_history" || got.ThroughputEstimate.TrainingSamples != 0 {
		t.Fatalf("imported labels admitted: %+v err=%v", got.ThroughputEstimate, err)
	}
}

func BenchmarkArrivalEstimateIndexedRead(b *testing.B) {
	database, target, history := storeEstimatorFixture(b)
	ctx := context.Background()
	// Same cohort and day, but legacy rows do not grow the observation index.
	noise := make([]model.Event, 10000)
	for i := range noise {
		e := history[0]
		e.AttemptID, e.ProxyRequestID = fmt.Sprintf("%032x", 2000+i), fmt.Sprintf("%032x", 2000+i)
		e.OutputTiming = nil
		noise[i] = e
	}
	for start := 0; start < len(noise); start += 500 {
		if err := database.WriteBatch(ctx, noise[start:start+500]); err != nil {
			b.Fatal(err)
		}
	}
	query := v2Query(model.OperationEvents, target.RequestedAt, target.RequestedAt.Add(time.Minute))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, found, err := database.EventByAttemptID(ctx, target.AttemptID, query)
		if err != nil || !found || got.ThroughputEstimate.Status != "estimated" {
			b.Fatalf("estimate=%+v err=%v", got.ThroughputEstimate, err)
		}
	}
}

func TestThroughputEstimateHistoryUsesBoundedCohortCompletionIndex(t *testing.T) {
	database, target, history := storeEstimatorFixture(t)
	ctx := context.Background()
	more := make([]model.Event, 0, 600)
	for i := 0; i < 600; i++ {
		e := history[0]
		e.AttemptID, e.ProxyRequestID = fmt.Sprintf("%032x", 2000+i), fmt.Sprintf("%032x", 2000+i)
		e.RequestedAt = target.RequestedAt.Add(-time.Duration(i+2) * time.Minute)
		if i < 250 {
			e.ProxyRequestID = history[0].ProxyRequestID
		}
		more = append(more, e)
	}
	if err := database.WriteBatch(ctx, more); err != nil {
		t.Fatal(err)
	}
	arguments := arrivalEstimateArguments(target)
	rows, err := database.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+arrivalEstimateHistorySQL, arguments...)
	if err != nil {
		t.Fatal(err)
	}
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "SEARCH events USING INDEX events_arrival_estimate_idx") || !strings.Contains(plan, "<expr>>? AND <expr><?") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("history is not an ordered cohort/completion index seek:\n%s", plan)
	}
	got, err := database.arrivalEstimateHistory(ctx, target, arguments)
	if err != nil || len(got) != 200 {
		t.Fatalf("bounded history=%d err=%v", len(got), err)
	}
	seen := map[string]bool{}
	for i, e := range got {
		if seen[e.ProxyRequestID] {
			t.Fatal("duplicate request exhausted history budget")
		}
		seen[e.ProxyRequestID] = true
		if i > 0 {
			previous, _ := model.ArrivalEstimateCompletion(got[i-1])
			current, _ := model.ArrivalEstimateCompletion(e)
			if current.After(previous) {
				t.Fatal("history not ordered by completion")
			}
		}
	}
	pure := model.EstimateArrivalThroughput(target, append(history, more...))
	stored := model.EstimateArrivalThroughput(target, got)
	if !reflect.DeepEqual(pure, stored) {
		t.Fatalf("bounded reader differs from pure policy: %+v vs %+v", stored, pure)
	}
}

func TestThroughputEstimateIndexMigrationUpgradesV13(t *testing.T) {
	database, target, _ := storeEstimatorFixture(t)
	ctx := context.Background()
	config := database.config
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `ALTER TABLE events DROP COLUMN cancellation_source; ALTER TABLE events DROP COLUMN failure_hop; ALTER TABLE events DROP COLUMN outcome; DROP INDEX IF EXISTS events_session_id_idx; ALTER TABLE events DROP COLUMN session_id; ALTER TABLE events DROP COLUMN pricing_breakdown; ALTER TABLE events DROP COLUMN provider_credits; ALTER TABLE events DROP COLUMN price_usd_per_credit_nano; ALTER TABLE pricing_manual_rules DROP COLUMN service_tier_prices; ALTER TABLE pricing_catalog_rules DROP COLUMN service_tier_prices; DROP INDEX events_arrival_estimate_idx; DELETE FROM schema_migrations WHERE version>=14`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upgraded.Close(ctx) })
	if upgraded.SchemaVersion() != latestSchemaVersion(t) {
		t.Fatalf("schema=%d", upgraded.SchemaVersion())
	}
	if _, err := os.Stat(config.Path + ".pre-migration-v14"); err != nil {
		t.Fatalf("migration backup: %v", err)
	}
	query := v2Query(model.OperationEvents, target.RequestedAt, target.RequestedAt.Add(time.Minute))
	got, found, err := upgraded.EventByAttemptID(ctx, target.AttemptID, query)
	if err != nil || !found || got.ThroughputEstimate == nil || got.ThroughputEstimate.Status != "estimated" {
		t.Fatalf("migration estimate=%+v found=%v err=%v", got.ThroughputEstimate, found, err)
	}
}

func TestThroughputEstimateStoreMatchesCausalValidationPolicy(t *testing.T) {
	database, target, history := storeEstimatorFixture(t)
	ctx := context.Background()
	for i := 50; i < 70; i++ {
		history[i].RequestedAt = target.RequestedAt.Add(time.Duration(i-110) * time.Minute)
		history[i].LatencyMS = int64(30 * time.Minute / time.Millisecond)
		if _, err := database.db.ExecContext(ctx, `UPDATE events SET requested_at_ns=?,latency_ms=? WHERE attempt_id=?`, history[i].RequestedAt.UnixNano(), history[i].LatencyMS, history[i].AttemptID); err != nil {
			t.Fatal(err)
		}
	}
	query := v2Query(model.OperationEvents, target.RequestedAt, target.RequestedAt.Add(time.Minute))
	got, found, err := database.EventByAttemptID(ctx, target.AttemptID, query)
	want := model.EstimateArrivalThroughput(target, history)
	if err != nil || !found || want.Reason != "insufficient_history" || want.TrainingSamples != 40 || got.ThroughputEstimate == nil || !reflect.DeepEqual(*got.ThroughputEstimate, want) {
		t.Fatalf("causal store=%+v pure=%+v found=%v err=%v", got.ThroughputEstimate, want, found, err)
	}
}

func TestThroughputEstimateStoreAsOfIndependentOfFiltersAndPagination(t *testing.T) {
	database, target, _ := storeEstimatorFixture(t)
	ctx := context.Background()
	query := v2Query(model.OperationEvents, target.RequestedAt.Add(-time.Hour), target.RequestedAt.Add(time.Minute))
	query.PageSize = 1
	page, err := database.Events(ctx, query)
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	got := page.Events[0]
	if got.ThroughputEstimate == nil || got.ThroughputEstimate.Status != "estimated" || *got.ThroughputEstimate.TokensPerSecond != 1500 || got.TokensPerSecond != nil || got.SpeedEstimated {
		t.Fatalf("target=%+v", got)
	}
	detail, found, err := database.EventByAttemptID(ctx, target.AttemptID, query)
	if err != nil || !found || !reflect.DeepEqual(got.ThroughputEstimate, detail.ThroughputEstimate) {
		t.Fatalf("detail=%+v found=%v err=%v", detail, found, err)
	}
	query.Start = target.RequestedAt
	query.PageSize = 100
	query.KeyIDs = []string{target.KeyID}
	query.Filters = map[string]json.RawMessage{"provider": json.RawMessage(`"` + target.Provider + `"`)}
	filtered, err := database.Events(ctx, query)
	if err != nil || len(filtered.Events) != 1 || !reflect.DeepEqual(got.ThroughputEstimate, filtered.Events[0].ThroughputEstimate) {
		t.Fatalf("filtered=%+v err=%v", filtered, err)
	}
	query.Filters = nil
	query.Start = target.RequestedAt.Add(-2 * time.Hour)
	query.PageSize = 100
	all, err := database.Events(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range all.Events[1:] {
		if event.ThroughputEstimate != nil || event.TokensPerSecond == nil || *event.TokensPerSecond != 1500 {
			t.Fatalf("observation changed=%+v", event)
		}
	}
}

func TestThroughputEstimateStoreKeyPrivacyAndReadFailure(t *testing.T) {
	database, target, _ := storeEstimatorFixture(t)
	ctx := context.Background()
	other := target
	other.AttemptID, other.ProxyRequestID, other.KeyID = strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("6", 64)
	if err := database.WriteBatch(ctx, []model.Event{other}); err != nil {
		t.Fatal(err)
	}
	query := v2Query(model.OperationEvents, target.RequestedAt, target.RequestedAt.Add(time.Minute))
	got, found, err := database.EventByAttemptID(ctx, other.AttemptID, query)
	if err != nil || !found || got.ThroughputEstimate == nil || got.ThroughputEstimate.Reason != "insufficient_history" || got.ThroughputEstimate.TrainingSamples != 0 || got.ThroughputEstimate.ValidationSamples != 0 {
		t.Fatalf("cross-key=%+v found=%v err=%v", got, found, err)
	}
	if _, err := database.db.ExecContext(ctx, `DROP INDEX events_arrival_estimate_idx`); err != nil {
		t.Fatal(err)
	}
	got, found, err = database.EventByAttemptID(ctx, target.AttemptID, query)
	if err != nil || !found || got.Tokens.Output != target.Tokens.Output || got.ThroughputEstimate == nil || got.ThroughputEstimate.Reason != "history_unavailable" {
		t.Fatalf("history failure=%+v found=%v err=%v", got, found, err)
	}
}

func TestThroughputEstimateMalformedOptionalTimingPreservesAccounting(t *testing.T) {
	for name, payload := range map[string]string{
		"wrong_type":          `{"version":"bad"}`,
		"unsupported_version": `{"version":99}`,
	} {
		t.Run(name, func(t *testing.T) {
			database, target, _ := storeEstimatorFixture(t)
			ctx := context.Background()
			if _, err := database.db.ExecContext(ctx, `UPDATE events SET output_timing=? WHERE attempt_id=?`, payload, target.AttemptID); err != nil {
				t.Fatal(err)
			}
			query := v2Query(model.OperationEvents, target.RequestedAt, target.RequestedAt.Add(time.Minute))
			got, found, err := database.EventByAttemptID(ctx, target.AttemptID, query)
			if err != nil || !found || got.Tokens != target.Tokens || got.OutputTiming != nil || got.ThroughputEstimate == nil || got.ThroughputEstimate.Reason != "ineligible_metadata" {
				t.Fatalf("malformed metadata=%+v found=%v err=%v", got, found, err)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("accounting event cannot round-trip after discarding malformed timing: %v", err)
			}
		})
	}
}
