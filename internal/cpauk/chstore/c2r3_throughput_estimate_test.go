package chstore

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func storeEstimatorFixture(t testing.TB) (*Store, model.Event, []model.Event) {
	t.Helper()
	ctx := context.Background()
	database, err := openR3Store(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
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
	t.Parallel()
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
	query := r3V2Query(model.OperationEvents, target.RequestedAt, target.RequestedAt.Add(time.Minute))
	got, found, err := database.EventByAttemptID(ctx, target.AttemptID, query)
	if err != nil || !found || got.ThroughputEstimate == nil || got.ThroughputEstimate.Method != "cohort_median_v1" || *got.ThroughputEstimate.TokensPerSecond != 1500 {
		t.Fatalf("import retained forged estimate: %+v found=%v err=%v", got.ThroughputEstimate, found, err)
	}
	// Replacing the native pool with imported copies must remove every label.
	if _, err := r3ExecUpdate(ctx, database, `UPDATE events SET import_batch_id='imported-history' WHERE requested_at_ns < ?`, target.RequestedAt.UnixNano()); err != nil {
		t.Fatal(err)
	}
	got, found, err = database.EventByAttemptID(ctx, target.AttemptID, query)
	if err != nil || !found || got.ThroughputEstimate == nil || got.ThroughputEstimate.Reason != "insufficient_history" || got.ThroughputEstimate.TrainingSamples != 0 {
		t.Fatalf("imported labels admitted: %+v err=%v", got.ThroughputEstimate, err)
	}
}

func TestThroughputEstimateStoreMatchesCausalValidationPolicy(t *testing.T) {
	t.Parallel()
	database, target, history := storeEstimatorFixture(t)
	ctx := context.Background()
	// Event time is part of the ClickHouse key. Build the altered causal
	// fixture by replacing its rows, rather than mutating an immutable key.
	ids := make([]string, 0, 20)
	for i := 50; i < 70; i++ {
		history[i].RequestedAt = target.RequestedAt.Add(time.Duration(i-110) * time.Minute)
		history[i].LatencyMS = int64(30 * time.Minute / time.Millisecond)
		ids = append(ids, history[i].AttemptID)
	}
	if _, err := r3ExecUpdate(ctx, database, `ALTER TABLE PLACEHOLDER DELETE WHERE attempt_id IN (?) SETTINGS mutations_sync=2`, ids); err != nil {
		t.Fatal(err)
	}
	if err := database.WriteBatch(ctx, history[50:70]); err != nil {
		t.Fatal(err)
	}

	query := r3V2Query(model.OperationEvents, target.RequestedAt, target.RequestedAt.Add(time.Minute))
	got, found, err := database.EventByAttemptID(ctx, target.AttemptID, query)
	want := model.EstimateArrivalThroughput(target, history)
	if err != nil || !found || want.Reason != "insufficient_history" || want.TrainingSamples != 40 || got.ThroughputEstimate == nil || !reflect.DeepEqual(*got.ThroughputEstimate, want) {
		t.Fatalf("causal store=%+v pure=%+v found=%v err=%v", got.ThroughputEstimate, want, found, err)
	}
}

func TestThroughputEstimateStoreAsOfIndependentOfFiltersAndPagination(t *testing.T) {
	t.Parallel()
	database, target, _ := storeEstimatorFixture(t)
	ctx := context.Background()
	query := r3V2Query(model.OperationEvents, target.RequestedAt.Add(-time.Hour), target.RequestedAt.Add(time.Minute))
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
	t.Parallel()
	database, target, _ := storeEstimatorFixture(t)
	ctx := context.Background()
	other := target
	other.AttemptID, other.ProxyRequestID, other.KeyID = strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("6", 64)
	if err := database.WriteBatch(ctx, []model.Event{other}); err != nil {
		t.Fatal(err)
	}
	query := r3V2Query(model.OperationEvents, target.RequestedAt, target.RequestedAt.Add(time.Minute))
	got, found, err := database.EventByAttemptID(ctx, other.AttemptID, query)
	if err != nil || !found || got.ThroughputEstimate == nil || got.ThroughputEstimate.Reason != "insufficient_history" || got.ThroughputEstimate.TrainingSamples != 0 || got.ThroughputEstimate.ValidationSamples != 0 {
		t.Fatalf("cross-key=%+v found=%v err=%v", got, found, err)
	}
	if _, err := r3ExecUpdate(ctx, database, `ALTER TABLE PLACEHOLDER RENAME COLUMN output_timing TO hidden_output_timing`); err != nil {
		t.Fatal(err)
	}
	got, found, err = database.EventByAttemptID(ctx, target.AttemptID, query)
	if err != nil || !found || got.Tokens.Output != target.Tokens.Output || got.ThroughputEstimate == nil || got.ThroughputEstimate.Reason != "history_unavailable" {
		t.Fatalf("history failure=%+v found=%v err=%v", got, found, err)
	}
}

func r3ExecUpdate(ctx context.Context, s *Store, statement string, args ...any) (int, error) {
	statement = strings.ReplaceAll(statement, "PLACEHOLDER", s.table("analytics_events"))
	if strings.HasPrefix(statement, "UPDATE events SET ") {
		statement = "ALTER TABLE " + s.table("analytics_events") + " UPDATE " + strings.TrimPrefix(statement, "UPDATE events SET ") + " SETTINGS mutations_sync=2"
	}
	return 0, s.db.Conn().Exec(ctx, statement, args...)
}

// SQLite's observation index has both completion bounds and the independent
// requested-at floor. Long-running and future-completion rows cannot train.
func TestR3ThroughputSQLBoundsAndDuplicateBudget(t *testing.T) {
	t.Parallel()
	database, target, history := storeEstimatorFixture(t)
	ctx := context.Background()
	more := make([]model.Event, 0, 604)
	for i := 0; i < 600; i++ {
		e := history[0]
		e.AttemptID = fmt.Sprintf("%032x", 2000+i)
		e.ProxyRequestID = fmt.Sprintf("%032x", 2000+i)
		e.RequestedAt = target.RequestedAt.Add(-time.Duration(i+2) * time.Minute)
		if i < 250 {
			e.ProxyRequestID = history[0].ProxyRequestID
		}
		more = append(more, e)
	}
	for i := 0; i < 4; i++ {
		e := history[0]
		e.AttemptID = fmt.Sprintf("%032x", 5000+i)
		e.ProxyRequestID = e.AttemptID
		switch i {
		case 0:
			e.RequestedAt = target.RequestedAt.Add(-25 * time.Hour)
			e.LatencyMS = int64(24 * time.Hour / time.Millisecond)
		case 1:
			e.RequestedAt = target.RequestedAt.Add(-time.Minute)
			e.LatencyMS = int64(2 * time.Minute / time.Millisecond)
		case 2:
			e.RequestedAt = target.RequestedAt.Add(-24 * time.Hour)
			e.LatencyMS = 1
		case 3:
			e.RequestedAt = target.RequestedAt.Add(-time.Second)
			e.LatencyMS = 1000
		}
		more = append(more, e)
	}
	if err := database.WriteBatch(ctx, more); err != nil {
		t.Fatal(err)
	}
	database.mu.RLock()
	got, err := database.arrivalEstimateHistory(ctx, target)
	database.mu.RUnlock()
	if err != nil || len(got) != 200 {
		t.Fatalf("history=%d err=%v", len(got), err)
	}
	seen := map[string]bool{}
	for i, e := range got {
		if seen[e.ProxyRequestID] {
			t.Fatal("duplicate request consumed bounded history")
		}
		seen[e.ProxyRequestID] = true
		if i > 0 {
			before, _ := model.ArrivalEstimateCompletion(got[i-1])
			at, _ := model.ArrivalEstimateCompletion(e)
			if at.After(before) {
				t.Fatal("history not in completion order")
			}
		}
	}
	want := model.EstimateArrivalThroughput(target, append(history, more...))
	actual := model.EstimateArrivalThroughput(target, got)
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("bounded=%+v pure=%+v", actual, want)
	}
}

func TestThroughputEstimateMalformedOptionalTimingPreservesAccounting(t *testing.T) {
	t.Parallel()
	for name, payload := range map[string]string{
		"wrong_type":          `{"version":"bad"}`,
		"unsupported_version": `{"version":99}`,
	} {
		t.Run(name, func(t *testing.T) {
			database, target, _ := storeEstimatorFixture(t)
			ctx := context.Background()
			if _, err := r3ExecUpdate(ctx, database, `UPDATE events SET output_timing=? WHERE attempt_id=?`, payload, target.AttemptID); err != nil {
				t.Fatal(err)
			}
			query := r3V2Query(model.OperationEvents, target.RequestedAt, target.RequestedAt.Add(time.Minute))
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
