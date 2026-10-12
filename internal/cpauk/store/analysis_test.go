package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestAnalysisTimingMetricsMatchesGenericMetrics(t *testing.T) {
	database, events := openV2FixtureStore(t)
	query := v2Query(model.OperationAnalysis, events[0].RequestedAt, events[0].RequestedAt.Add(15*time.Minute))
	where, arguments, err := buildWhere(query)
	if err != nil {
		t.Fatal(err)
	}

	database.mu.RLock()
	got, gotErr := database.analysisTimingMetrics(context.Background(), where, arguments)
	want, wantErr := database.timingMetrics(context.Background(), query, true)
	database.mu.RUnlock()
	if gotErr != nil {
		t.Fatal(gotErr)
	}
	if wantErr != nil {
		t.Fatal(wantErr)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("analysis timing metrics differ from generic metrics:\n got=%+v\nwant=%+v", got, want)
	}
}
