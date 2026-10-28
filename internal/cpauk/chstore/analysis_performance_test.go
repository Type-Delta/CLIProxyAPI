package chstore

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	sqlstore "github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

func TestAnalysisGroupedRetainedAccounting(t *testing.T) {
	t.Parallel()
	s, events := r3OpenV2FixtureStore(t)
	ctx := context.Background()
	q := r3V2Query(model.OperationAnalysis, events[0].RequestedAt.Truncate(24*time.Hour), events[0].RequestedAt.Truncate(24*time.Hour).Add(24*time.Hour))
	q.BucketWidth = "1h"
	if _, err := s.ApplyRetention(ctx, q.End, 100); err != nil {
		t.Fatal(err)
	}
	var series model.AnalysisSeriesByCategory
	got, err := s.analysisAccounting(ctx, q, &series)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 1 || got.Models[0].Requests != 2 || len(series.Buckets) != 24 {
		t.Fatalf("models=%+v series buckets=%d", got.Models, len(series.Buckets))
	}
}

// Retained identities repeat across models and buckets; selection predicates
// must use stored values before identity-only rows receive zero metric values.
func TestAnalysisGroupedRetainedModelsMatchSQLite(t *testing.T) {
	t.Parallel()
	events := syntheticDifferentialEvents(t)
	for i := range events {
		events[i].Model = fmt.Sprintf("model-%02d", i%12)
	}
	f := openDifferentialFixture(t, events, 0)
	start := events[0].RequestedAt.Truncate(24 * time.Hour)
	raw := start.Add(3 * 24 * time.Hour)
	if _, err := f.sqlite.ApplyRetentionPolicy(context.Background(), raw, time.Time{}, 1000); err != nil {
		t.Fatal(err)
	}
	f.copyRetained(t, raw, time.Time{})
	reader := diffReader(model.OperationAnalysis, (*Store).Analysis, (*sqlstore.SQLiteStore).Analysis)
	for _, success := range []string{"", "true", "false"} {
		t.Run("success="+success, func(t *testing.T) {
			q := model.Query{SchemaVersion: 2, Start: start, End: start.Add(6 * 24 * time.Hour), TimeZone: "UTC", BucketWidth: "1d"}
			if success != "" {
				q.Filters = map[string]json.RawMessage{"success": json.RawMessage(success)}
			}
			f.compare(t, reader, q)
		})
	}
}
