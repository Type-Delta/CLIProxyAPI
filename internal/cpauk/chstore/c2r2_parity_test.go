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

func TestC2R2AwardPercentilesAndLegacyAccounting(t *testing.T) {
	t.Parallel()
	events := []model.Event{}
	for i := 1; i <= 210; i++ {
		event := c2r2LeaderboardEvent(i, "nearest-rank", "provider-a", true)
		event.Tokens.Output = int64(i * 100)
		event.Tokens.Reasoning = int64(i)
		event.Tokens.Total = event.Tokens.Input + event.Tokens.Output
		event.OutputTiming = &model.OutputTiming{Version: 1, Protocol: "chat", Source: "source_reader", Status: "observed", FirstArrivalNS: 1, LastArrivalNS: 1000000001, ArrivalCount: 3, WindowTokens: int64(i * 100), TokenQuality: model.TokenQualityEstimated, Coverage: "visible_deltas"}
		if i%5 == 0 {
			event.Outcome = "canceled"
		}
		events = append(events, event)
	}
	f := openDifferentialFixture(t, events, 0)
	q := c2r2LeaderboardQuery()
	f.compare(t, diffReader(model.OperationModelLeaderboard, (*Store).ModelLeaderboard, (*sqlstore.SQLiteStore).ModelLeaderboard), q)
	// The legacy schema counts separate output and reasoning categories together.
	if err := f.ch.db.Conn().Exec(context.Background(), "ALTER TABLE "+f.ch.table("analytics_events")+" UPDATE accounting_schema='separate-output-v1' WHERE 1 SETTINGS mutations_sync=2"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.legacy.Exec("UPDATE events SET accounting_schema='separate-output-v1'"); err != nil {
		t.Fatal(err)
	}
	f.compare(t, diffReader(model.OperationModelLeaderboard, (*Store).ModelLeaderboard, (*sqlstore.SQLiteStore).ModelLeaderboard), q)
}

func TestC2R2ModelCostWidthsAndUnpriced(t *testing.T) {
	t.Parallel()
	events := syntheticDifferentialEvents(t)[:24]
	for i := range events {
		events[i].AttemptID = fmt.Sprintf("%032x", i+70000)
		events[i].Provider = []string{"<provider>", "provider/one", "&provider", "provider\u2028two"}[i%4]
		events[i].RequestedAt = time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC).Add(time.Duration(i) * 10 * time.Minute)
		if i%3 == 0 {
			events[i].Model = "unpriced"
		}
	}
	f := openDifferentialFixture(t, events, 0)
	// Legacy scalar data can predate intake's trimmed-tier validation.
	tier := "\u00a0\tPRIORITY\u3000\n"
	if err := f.ch.db.Conn().Exec(context.Background(), "ALTER TABLE "+f.ch.table("analytics_events")+" UPDATE service_tier_used=? WHERE model='unpriced' SETTINGS mutations_sync=2", tier); err != nil {
		t.Fatal(err)
	}
	if _, err := f.legacy.Exec("UPDATE events SET service_tier_used=? WHERE model='unpriced'", tier); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{events[1].Model, "unpriced"} {
		for _, zone := range []string{"UTC", "Asia/Kathmandu", "Australia/Lord_Howe"} {
			for _, width := range []string{"", "5m", "1h", "1d", "1w"} {
				t.Run(name+"/"+zone+"/"+width, func(t *testing.T) {
					filter, _ := json.Marshal([]string{name})
					q := model.Query{SchemaVersion: 2, Operation: model.OperationModelCostSeries, Start: events[0].RequestedAt, End: events[len(events)-1].RequestedAt.Add(time.Minute), TimeZone: zone, BucketWidth: width, Filters: map[string]json.RawMessage{"model": filter}}
					// Call directly so the differential harness does not supply a default width.
					got, e1 := f.ch.ModelCostSeries(context.Background(), q)
					want, e2 := f.sqlite.ModelCostSeries(context.Background(), q)
					if e1 != nil || e2 != nil {
						t.Fatalf("CH %v SQLite %v", e1, e2)
					}
					a, _ := json.Marshal(got)
					b, _ := json.Marshal(want)
					if string(a) != string(b) {
						t.Fatalf("CH %s\nSQLite %s", a, b)
					}
				})
			}
		}
	}
	// Reconstruct absent snapshots with slash/HTML/unicode provider identifiers.
	if err := f.ch.db.Conn().Exec(context.Background(), "ALTER TABLE "+f.ch.table("analytics_events")+" UPDATE pricing_breakdown=NULL WHERE 1 SETTINGS mutations_sync=2"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.legacy.Exec("UPDATE events SET pricing_breakdown=NULL"); err != nil {
		t.Fatal(err)
	}
	filter, _ := json.Marshal([]string{events[1].Model})
	f.compare(t, diffReader(model.OperationModelCostSeries, (*Store).ModelCostSeries, (*sqlstore.SQLiteStore).ModelCostSeries), model.Query{SchemaVersion: 2, Start: events[0].RequestedAt, End: events[len(events)-1].RequestedAt.Add(time.Minute), TimeZone: "UTC", BucketWidth: "1h", Filters: map[string]json.RawMessage{"model": filter}})

}

func TestC2R2ModelCostRecordedEdgeCases(t *testing.T) {
	t.Parallel()
	events := syntheticDifferentialEvents(t)[:6]
	for i := range events {
		events[i].RequestedAt = events[0].RequestedAt.Add(time.Duration(i) * time.Minute)
	}
	f := openDifferentialFixture(t, events, 0)
	reader := diffReader(model.OperationModelCostSeries, (*Store).ModelCostSeries, (*sqlstore.SQLiteStore).ModelCostSeries)
	filter, _ := json.Marshal([]string{events[0].Model})
	q := model.Query{SchemaVersion: 2, Start: events[0].RequestedAt, End: events[0].RequestedAt.Add(time.Hour), TimeZone: "UTC", BucketWidth: "1h", Filters: map[string]json.RawMessage{"model": filter}}
	changes := []string{
		// This exceeds exact Float64 integer precision. Every money operation must
		// preserve the nano-USD value, including the JSON decimal conversion.
		`known_cost_nano=9007199254740993,pricing_breakdown='{"input":{"cost_usd":"9007199.254740993","rate_per_million_usd":"7/3"},"service_tier":"custom","input_tier_multiplier":"2"}'`,
		`known_cost_nano=1`, // A mismatched breakdown clears rates/costs, keeps tier/multiplier.
		`known_cost_nano=NULL`,
		`price_source='kiro-credits',known_cost_nano=12345`,
		`price_source=NULL,pricing_breakdown=NULL`,
		`known_cost_nano=0,total_tokens=0`, // No chart layer and no partial-pricing marker.
	}
	for i, change := range changes {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if err := f.ch.db.Conn().Exec(context.Background(), "ALTER TABLE "+f.ch.table("analytics_events")+" UPDATE "+change+" WHERE 1 SETTINGS mutations_sync=2"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.legacy.Exec("UPDATE events SET " + change); err != nil {
				t.Fatal(err)
			}
			f.compare(t, reader, q)
		})
	}
}
