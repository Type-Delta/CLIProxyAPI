package chstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestS2ReadBenchmark(t *testing.T) {
	if os.Getenv("CPA_S2_BENCH") != "1" {
		t.Skip("set CPA_S2_BENCH=1")
	}
	ctx := context.Background()
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 4 << 30, PriceBook: fixturePriceBook()})
	end := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	const count = 200000
	for base := 0; base < count; base += 5000 {
		events := make([]model.Event, 5000)
		patches := make([]model.ProxyResponsePatch, 0, 1500)
		for j := range events {
			i := base + j
			at := end.Add(-10 * 24 * time.Hour).Add(time.Duration(i) * (10 * 24 * time.Hour / count))
			e := throughputStoreEvent(fmt.Sprintf("%032x", i+1), model.KeyID("s2-key"), at, 200, 500, 1000, 480)
			e.AttemptID, e.ProxyRequestID = fmt.Sprintf("%032x", i+1), fmt.Sprintf("%032x", i+1)
			e.Tokens.Total = e.Tokens.Input + e.Tokens.Output
			e.Model = fmt.Sprintf("model-%02d", i%8)
			e.Source = "native"
			e.Stream = new(true)
			e.OutputTiming = &model.OutputTiming{Version: 1, Protocol: "responses", Source: "source_reader", Coverage: "visible_deltas", Status: "observed", FirstArrivalNS: 1, LastArrivalNS: int64(time.Second) + 1, WindowTokens: 1500, ArrivalCount: 10, TokenQuality: model.TokenQualityEstimated}
			if i >= count-50 {
				e.OutputTiming.Status = "unavailable"
				e.OutputTiming.Reason = "insufficient_tokens"
				e.OutputTiming.WindowTokens = 0
				e.OutputTiming.TokenQuality = model.TokenQualityMissing
			}
			events[j] = e
			if i%3 == 0 {
				patches = append(patches, model.ProxyResponsePatch{ProxyRequestID: e.ProxyRequestID, StatusCode: 200, RespondedAt: at.Add(time.Second)})
			}
		}
		if err := s.WriteBatchWithPatches(ctx, events, patches); err != nil {
			t.Fatal(err)
		}
	}
	var retention RetentionResult
	if err := s.c4bPromote(ctx, end.Add(-2*24*time.Hour), false, &retention); err != nil {
		t.Fatal(err)
	}
	if err := s.c4bCleanup(ctx, s.retentionCutoff.UnixNano(), false); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"analytics_events", "analytics_outcomes", "analytics_rollups", "analytics_request_rollups", "analytics_retention_heads"} {
		if err := s.db.Conn().Exec(ctx, "OPTIMIZE TABLE "+s.table(table)+" FINAL"); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"summary_1d", "summary_7d", "events", "model_leaderboard", "events_estimates"} {
		q := model.Query{SchemaVersion: 2, Operation: model.OperationSummary, Start: end.Add(-24 * time.Hour), End: end, TimeZone: "UTC"}
		if name == "summary_7d" {
			q.Start = end.Add(-7 * 24 * time.Hour)
		}
		id := fmt.Sprintf("s2-%s-%d", name, time.Now().UnixNano())
		c := ch.Context(ctx, ch.WithSettings(ch.Settings{"log_comment": id}))
		var result any
		var err error
		started := time.Now()
		switch name {
		case "events", "events_estimates":
			q.Operation = model.OperationEvents
			q.PageSize = 50
			if name == "events" {
				q.End = end.Add(-time.Hour)
			}
			if os.Getenv("CPA_S2_LEGACY") == "1" {
				result, err = s.s2LegacyEvents(c, q, false)
			} else {
				result, err = s.Events(c, q)
			}
		case "model_leaderboard":
			q.Operation = model.OperationModelLeaderboard
			if os.Getenv("CPA_S2_LEGACY") == "1" {
				result, err = s.s2LegacyModelLeaderboard(c, q)
			} else {
				result, err = s.ModelLeaderboard(c, q)
			}
		default:
			if os.Getenv("CPA_S2_LEGACY") == "1" {
				result, err = s.s2LegacySummary(c, q)
			} else {
				result, err = s.Summary(c, q)
			}
		}
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if root := os.Getenv("CPA_S2_RESULTS"); root != "" {
			raw, e := json.MarshalIndent(result, "", "  ")
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(root, name+".json"), raw, 0600); e != nil {
				t.Fatal(e)
			}
		}
		if err := s.db.Conn().Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
			t.Fatal(err)
		}
		var queries, reads uint64
		if err := s.db.Conn().QueryRow(ctx, "SELECT count(),sum(read_rows) FROM system.query_log WHERE type='QueryFinish' AND log_comment=?", id).Scan(&queries, &reads); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s queries=%d read_rows=%d elapsed=%s", name, queries, reads, elapsed)
		if os.Getenv("CPA_S2_LEGACY") != "1" {
			budget := map[string]uint64{"summary_1d": 4, "summary_7d": 6, "events": 4, "model_leaderboard": 2, "events_estimates": 5}[name]
			if queries > budget {
				t.Errorf("%s query budget: got %d want <=%d", name, queries, budget)
			}
		}
		if root := os.Getenv("CPA_S2_COMPARE"); root != "" {
			before, err := os.ReadFile(filepath.Join(root, name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			after, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			normalize := func(raw []byte) any {
				var value map[string]any
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Fatal(err)
				}
				if meta, ok := value["meta"].(map[string]any); ok {
					if _, ok := meta["next_cursor"]; ok {
						meta["next_cursor"] = "present"
					}
				}
				return value
			}
			if !reflect.DeepEqual(normalize(before), normalize(after)) {
				t.Errorf("%s result differs from legacy baseline", name)
			}
		}

	}
}
