package chstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/importer"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestC4bLateImportIsVisibleBelowRawFloor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	cutoff := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	if _, err := s.ApplyRetentionPolicy(ctx, cutoff, cutoff.AddDate(0, 0, -1), 1); err != nil {
		t.Fatal(err)
	}
	event := loadFixtureEvents(t)[0]
	if inserted, err := s.WriteImportBatch(ctx, []model.Event{event}, "late-import"); err != nil || inserted != 1 {
		t.Fatalf("inserted=%d err=%v", inserted, err)
	}
	if got := c4bVisible(t, s); got.Attempts != 1 {
		t.Fatalf("late imported attempt silently hidden: %+v", got)
	}
	if _, err := s.ApplyRetentionPolicy(ctx, cutoff, cutoff.AddDate(0, 0, -1), 1); err != nil {
		t.Fatal(err)
	}
	if got := c4bVisible(t, s); got.Attempts != 1 {
		t.Fatalf("late imported attempt lost by cleanup: %+v", got)
	}
}

func TestC4bLateImportAndLiveMatchSQLiteAcrossGrains(t *testing.T) {
	t.Parallel()
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprint("live-", live), func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t)
			sqlite, err := store.Open(ctx, Config{Path: filepath.Join(t.TempDir(), "sqlite.db"), MaxStorageBytes: 64 << 20, PriceBook: fixturePriceBook()})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sqlite.Close(ctx) }()
			base := loadFixtureEvents(t)[0]
			base.RequestedAt = time.Date(2026, 8, 31, 4, 5, 0, 0, time.UTC)
			native := []model.Event{base, base}
			native[0].AttemptID = fmt.Sprintf("%032x", 701)
			native[1].AttemptID = fmt.Sprintf("%032x", 702)
			native[1].RequestedAt = time.Date(2026, 9, 2, 4, 5, 0, 0, time.UTC)
			if err = s.WriteBatch(ctx, native); err != nil {
				t.Fatal(err)
			}
			if err = sqlite.WriteBatch(ctx, native); err != nil {
				t.Fatal(err)
			}
			raw := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
			hourly := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			if _, err = s.ApplyRetentionPolicy(ctx, raw, hourly, 1); err != nil {
				t.Fatal(err)
			}
			if _, err = sqlite.ApplyRetentionPolicy(ctx, raw, hourly, 1); err != nil {
				t.Fatal(err)
			}
			q := model.Query{SchemaVersion: 1, Start: time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC), End: raw.AddDate(0, 0, 2), TimeZone: "UTC"}
			baseline := c4bQuery(t, sqlite, q)
			c4bAssertQuery(t, c4bQuery(t, s, q), baseline)
			late := []model.Event{native[0], native[0], native[1], native[1]}
			for i := range late {
				late[i].AttemptID = fmt.Sprintf("%032x", 801+i)
			}
			late[3].RequestedAt = raw.Add(time.Hour)
			if live {
				if err = s.WriteBatch(ctx, late); err != nil {
					t.Fatal(err)
				}
				if err = sqlite.WriteBatch(ctx, late); err != nil {
					t.Fatal(err)
				}
			} else {
				if inserted, err := s.WriteImportBatch(ctx, late[:1], "late-batch"); err != nil || inserted != 1 {
					t.Fatalf("first=%d err=%v", inserted, err)
				}
				if inserted, err := s.WriteImportBatch(ctx, late[1:], "late-batch"); err != nil || inserted != 3 {
					t.Fatalf("next=%d err=%v", inserted, err)
				}
				if _, err = sqlite.WriteImportBatch(ctx, late, "late-batch"); err != nil {
					t.Fatal(err)
				}
				if inserted, err := s.WriteImportBatch(ctx, late, "late-batch"); err != nil || inserted != 0 {
					t.Fatalf("replay=%d err=%v", inserted, err)
				}
			}
			if _, err = sqlite.ApplyRetentionPolicy(ctx, raw, hourly, 1); err != nil {
				t.Fatal(err)
			}
			c4bAssertQuery(t, c4bQuery(t, s, q), c4bQuery(t, sqlite, q))
			if _, err = s.ApplyRetentionPolicy(ctx, raw, hourly, 1); err != nil {
				t.Fatal(err)
			}
			c4bAssertQuery(t, c4bQuery(t, s, q), c4bQuery(t, sqlite, q))
			if !live {
				for _, grain := range []string{"hourly", "daily"} {
					var count uint64
					if err = s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table("analytics_rollups")+" FINAL WHERE grain=? AND import_batch_id='late-batch' AND is_deleted=0 AND "+s.c4bPublishedPredicate(), grain).Scan(&count); err != nil || count == 0 {
						t.Fatalf("%s count=%d err=%v", grain, count, err)
					}
				}
				if _, err = s.RollbackImport(ctx, "late-batch"); err != nil {
					t.Fatal(err)
				}
				if _, err = sqlite.RollbackImport(ctx, "late-batch"); err != nil {
					t.Fatal(err)
				}
				c4bAssertQuery(t, c4bQuery(t, s, q), baseline)
				c4bAssertQuery(t, c4bQuery(t, s, q), c4bQuery(t, sqlite, q))
				if inserted, err := s.WriteImportBatch(ctx, late, "late-batch"); err != nil || inserted != 4 {
					t.Fatalf("reinsert after rollback=%d err=%v", inserted, err)
				}
			}
		})
	}
}

func TestC4bLateWriteCrashGenerations(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"source", "totals", "requests", "staged", "published", "receipts"} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t)
			event := loadFixtureEvents(t)[0]
			raw := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
			hourly := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			if err := s.WriteBatch(ctx, []model.Event{event}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ApplyRetentionPolicy(ctx, raw, hourly, 1); err != nil {
				t.Fatal(err)
			}
			baseline := c4bVisible(t, s)
			late := event
			late.AttemptID = fmt.Sprintf("%032x", 900)
			stopped := errors.New("late write interrupted")
			interrupted := context.WithValue(ctx, c4bStepKey{}, func(name string) error {
				if name == "late:"+step {
					return stopped
				}
				return nil
			})
			if _, err := s.WriteImportBatch(interrupted, []model.Event{late}, "late-crash"); !errors.Is(err, stopped) {
				t.Fatalf("err=%v", err)
			}
			got := c4bVisible(t, s)
			want := baseline
			if step == "published" || step == "receipts" {
				want.Attempts++
				want.Tokens += event.Tokens.Total
				if event.KnownCost != nil {
					want.Money += int64(*event.KnownCost)
				} else {
					price, err := fixturePriceBook().Price(event)
					if err != nil {
						t.Fatal(err)
					}
					if price.KnownCost != nil {
						want.Money += int64(*price.KnownCost)
					}
				}
			}
			if got != want {
				t.Fatalf("interrupted=%+v want=%+v", got, want)
			}
			if _, err := s.ApplyRetentionPolicy(ctx, raw, hourly, 1); err != nil {
				t.Fatal(err)
			}
			after := c4bVisible(t, s)
			if after.Attempts != baseline.Attempts+1 || after.Requests != baseline.Requests {
				t.Fatalf("recovered=%+v baseline=%+v", after, baseline)
			}
			if inserted, err := s.WriteImportBatch(ctx, []model.Event{late}, "late-crash"); err != nil || inserted != 0 {
				t.Fatalf("replay=%d err=%v", inserted, err)
			}
		})
	}
}

func TestC4bLateCPAUKV115ImporterAndRollback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	raw := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	hourly := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.ApplyRetentionPolicy(ctx, raw, hourly, 1); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "source.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE usage_events (id INTEGER PRIMARY KEY,event_key TEXT NOT NULL,api_group_key TEXT NOT NULL,provider TEXT NOT NULL,endpoint TEXT NOT NULL,auth_type TEXT NOT NULL,request_id TEXT NOT NULL,model TEXT NOT NULL,model_alias TEXT,service_tier TEXT NOT NULL,response_service_tier TEXT NOT NULL,executor_type TEXT NOT NULL,timestamp TEXT NOT NULL,auth_index TEXT NOT NULL,failed INTEGER NOT NULL,generate INTEGER,latency_ms INTEGER NOT NULL,ttft_ms INTEGER,input_tokens INTEGER NOT NULL,output_tokens INTEGER NOT NULL,reasoning_tokens INTEGER NOT NULL,cached_tokens INTEGER NOT NULL,cache_read_tokens INTEGER NOT NULL,cache_creation_tokens INTEGER NOT NULL,total_tokens INTEGER NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO usage_events VALUES (1,'event','secret','openai','/v1/responses','oauth','request','gpt-test',NULL,'default','default','openai','2026-08-31T04:00:00Z','auth',0,1,25,5,10,0,0,0,0,0,10)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := importer.OpenCPAUKV115Source(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	worker := importer.Importer{Destination: s, Transform: importer.NewCPAUKV115Transformer(s.IdentityKeyArray(), false)}
	result, err := worker.RunWithBackup(ctx, source, importer.Options{BatchID: "late-v115", ChunkSize: 1}, filepath.Join(dir, "backup"))
	if err != nil || result.Inserted != 1 || !result.Reconciled {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if totals := c4bVisible(t, s); totals.Attempts != 1 || totals.Tokens != 10 || totals.Requests != 1 {
		t.Fatalf("imported totals=%+v", totals)
	}
	if _, err = s.ApplyRetentionPolicy(ctx, raw, hourly, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RollbackImport(ctx, "late-v115"); err != nil {
		t.Fatal(err)
	}
	if totals := c4bVisible(t, s); totals != (c4bTotals{}) {
		t.Fatalf("rollback totals=%+v", totals)
	}
}
