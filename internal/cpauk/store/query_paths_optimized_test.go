package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestTimeseriesMaterialized15MinuteMatchesRawAggregation(t *testing.T) {
	database, events := openV2FixtureStore(t)
	ctx := context.Background()
	query := v2Query(model.OperationTimeseries, events[0].RequestedAt.Truncate(15*time.Minute), events[0].RequestedAt.Truncate(15*time.Minute).Add(15*time.Minute))
	query.BucketWidth = "15m"

	rawQuery := query
	rawQuery.TimeZone = "Etc/UTC" // preserve UTC bucket boundaries while bypassing the materialized UTC fast path
	raw, err := database.Timeseries(ctx, rawQuery)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Rebuild15MinuteAggregates(ctx); err != nil {
		t.Fatal(err)
	}
	materialized, err := database.Timeseries(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(materialized.Points, raw.Points) {
		t.Fatalf("materialized timeseries differs from raw\nmaterialized=%+v\nraw=%+v", materialized, raw)
	}
}
