package chstore

import (
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	sqlstore "github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

func TestR3DifferentialActivityAnalysisVariants(t *testing.T) {
	t.Parallel()
	events := syntheticDifferentialEvents(t)
	// More than ten models exercises token-ranking ties and the per-bucket model
	// ordering independently from overall popularity.
	for i := range events {
		events[i].Model = fmt.Sprintf("model-%02d", i%12)
	}
	f := openDifferentialFixture(t, events, 0)
	for _, zone := range []string{"UTC", "America/New_York", "Asia/Kathmandu", "Australia/Lord_Howe"} {
		for _, window := range []string{"day", "week", "month", "year"} {
			t.Run(zone+"/"+window, func(t *testing.T) {
				q := model.Query{SchemaVersion: 2, Start: events[0].RequestedAt, End: events[len(events)-1].RequestedAt.Add(time.Hour), TimeZone: zone, Window: window}
				if window == "day" {
					q.End = q.Start.Add(24 * time.Hour)
				}
				f.compare(t, diffReader(model.OperationActivity, (*Store).Activity, (*sqlstore.SQLiteStore).Activity), q)
			})
		}
		for _, width := range []string{"5m", "30m", "1h", "1d", "1w"} {
			t.Run(zone+"/analysis/"+width, func(t *testing.T) {
				q := model.Query{SchemaVersion: 2, Start: events[0].RequestedAt, End: events[len(events)-1].RequestedAt.Add(time.Hour), TimeZone: zone, BucketWidth: width}
				f.compare(t, diffReader(model.OperationAnalysis, (*Store).Analysis, (*sqlstore.SQLiteStore).Analysis), q)
			})
		}
	}
}

func TestR3DifferentialScatterSamplingAndTies(t *testing.T) {
	t.Parallel()
	base := syntheticDifferentialEvents(t)[0]
	events := make([]model.Event, 1103)
	for i := range events {
		e := base
		e.AttemptID = fmt.Sprintf("%032x", i+100)
		e.ProxyRequestID = fmt.Sprintf("%032x", i/2+2000)
		e.RequestedAt = base.RequestedAt.Add(time.Duration(i/3) * time.Second)
		v := int64(i % 11)
		e.LatencyMS = v
		e.GenerationTimeMS = &v
		e.TimeToFirstTokenMS = &v
		events[i] = e
	}
	f := openDifferentialFixture(t, events, 0)
	q := model.Query{SchemaVersion: 2, Start: base.RequestedAt, End: base.RequestedAt.Add(time.Hour), TimeZone: "UTC"}
	f.compare(t, diffReader(model.OperationAnalysis, (*Store).Analysis, (*sqlstore.SQLiteStore).Analysis), q)
}
