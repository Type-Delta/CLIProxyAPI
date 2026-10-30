package chstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestS5AnalysisBenchmark(t *testing.T) {
	if os.Getenv("CPA_S5_BENCH") != "1" {
		t.Skip("set CPA_S5_BENCH=1")
	}
	ctx := context.Background()
	rate, output := model.NanoUSD(400001), model.NanoUSD(1700003)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "bench", Model: "model/v2", InputPerMillion: &rate, OutputPerMillion: &output, CacheReadMultiplier: "1/3", CacheCreationMultiplier: "1.25", Source: "fixture"}}}
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 4 << 30, PriceBook: book})
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	const count = 100000
	for base := 0; base < count; base += 5000 {
		events := make([]model.Event, 5000)
		for j := range events {
			i := base + j
			e := c2r2V2Event(fmt.Sprintf("%032x", i+1), fmt.Sprintf("%032x", i+1), model.KeyID("s5"), start.Add(time.Duration(i)*6*time.Second), true, nil, 0, 0, 10, 20)
			e.Model = "model/v2"
			e.Tokens.Input = int64(i + 20)
			e.Tokens.Output = int64(i % 997)
			e.Tokens.CacheRead = int64(i % 13)
			e.Tokens.CacheCreation = int64(i % 7)
			e.Tokens.Total = e.Tokens.Input + e.Tokens.Output
			events[j] = e
		}
		if err := s.WriteBatch(ctx, events); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.Conn().Exec(ctx, "OPTIMIZE TABLE "+s.table("analytics_events")+" FINAL"); err != nil {
		t.Fatal(err)
	}
	q := c2r2V2Query(model.OperationAnalysis, start, start.Add(7*24*time.Hour))
	var want model.AnalysisCostComponents
	for _, name := range []string{"before", "after"} {
		id := fmt.Sprintf("s5-%s-%d", name, time.Now().UnixNano())
		c := ch.Context(ctx, ch.WithSettings(ch.Settings{"log_comment": id}))
		begin := time.Now()
		var got model.AnalysisCostComponents
		var err error
		if name == "before" {
			got, err = s.s5LegacyAnalysisCosts(c, q)
			want = got
		} else {
			got, err = s.analysisCosts(c, q)
		}
		elapsed := time.Since(begin)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s differs: %+v vs %+v", name, got, want)
		}
		if err := s.db.Conn().Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
			t.Fatal(err)
		}
		var reads, groups, queries uint64
		if err := s.db.Conn().QueryRow(ctx, "SELECT sum(read_rows),sum(result_rows),count() FROM system.query_log WHERE type='QueryFinish' AND log_comment=?", id).Scan(&reads, &groups, &queries); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s events=%d rows_to_Go=%d read_rows=%d queries=%d wall=%s", name, count, groups, reads, queries, elapsed)
		if name == "after" && groups > 20 {
			t.Fatalf("cost rows to Go=%d; want bounded dimension groups", groups)
		}
	}
}
