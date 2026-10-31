package chstore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// ProfileInfo is sent once per SELECT, including empty results. Count actual
// native-protocol queries without a global query log or a fake analytics reader.
func historyQueryCounter() (context.Context, *atomic.Int64) {
	count := new(atomic.Int64)
	return ch.Context(context.Background(), ch.WithProfileInfo(func(*ch.ProfileInfo) { count.Add(1) })), count
}

func cacheFixture(t *testing.T) (*Store, model.Query, model.Event) {
	t.Helper()
	s := openTestStore(t)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return now }
	event := loadFixtureEvents(t)[0]
	event.RequestedAt = now.AddDate(0, 0, -3)
	if err := s.WriteBatch(context.Background(), []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	q := model.Query{SchemaVersion: 2, Operation: model.OperationSummary, Start: now.AddDate(0, 0, -4).Truncate(24 * time.Hour), End: now.AddDate(0, 0, -1).Truncate(24 * time.Hour), TimeZone: "UTC"}
	return s, q, event
}

func TestHistoryCacheReuseAndOwnership(t *testing.T) {
	t.Parallel()
	s, q, _ := cacheFixture(t)
	for _, op := range []model.Operation{model.OperationSummary, model.OperationTimeseries, model.OperationAnalysis, model.OperationActivity} {
		t.Run(string(op), func(t *testing.T) {
			q.Operation = op
			q.Window = ""
			q.BucketWidth = ""
			if op == model.OperationActivity {
				q.Window = "year"
			}
			if op == model.OperationTimeseries {
				q.BucketWidth = "1d"
			}
			ctx, count := historyQueryCounter()
			read := func() any {
				var value any
				var err error
				switch op {
				case model.OperationSummary:
					value, err = s.Summary(ctx, q)
				case model.OperationTimeseries:
					value, err = s.Timeseries(ctx, q)
				case model.OperationAnalysis:
					value, err = s.Analysis(ctx, q)
				case model.OperationActivity:
					value, err = s.Activity(ctx, q)
				}
				if err != nil {
					t.Fatal(err)
				}
				return value
			}
			first := read()
			cold := count.Load()
			if cold == 0 {
				t.Fatal("query counter did not observe cold read")
			}
			second := read()
			if count.Load() != cold {
				t.Fatalf("cached read issued %d SELECTs", count.Load()-cold)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatal("cached and uncached results differ")
			}
			if result, ok := second.(model.Activity); ok {
				result.Buckets[0].Requests = 999
				if !reflect.DeepEqual(first, read()) {
					t.Fatal("caller mutated cached activity")
				}
			}
			if result, ok := second.(model.Analysis); ok {
				result.Latency.Meta.Partial = true
				if !reflect.DeepEqual(first, read()) {
					t.Fatal("caller mutated cached analysis")
				}
			}
		})
	}
}

func TestHistoryCacheMutationClasses(t *testing.T) {
	for _, kind := range []string{"ingest", "import", "patch", "pricing", "pricing-provenance", "bindings", "reprice", "retention", "purge", "rollback", "restore", "epoch"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			s, q, event := cacheFixture(t)
			ctx, count := historyQueryCounter()
			if kind == "rollback" {
				event.AttemptID = fmt.Sprintf("%032x", 800)
				if _, err := s.WriteImportBatch(ctx, []model.Event{event}, "cache-import"); err != nil {
					t.Fatal(err)
				}
			}
			var backup string
			if kind == "restore" {
				backup = filepath.Join(t.TempDir(), "cache-backup")
				if _, err := s.Backup(ctx, backup); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Summary(ctx, q); err != nil {
				t.Fatal(err)
			}
			cold := count.Load()
			if _, err := s.Summary(ctx, q); err != nil {
				t.Fatal(err)
			}
			if count.Load() != cold {
				t.Fatal("baseline cache missed")
			}
			before := s.historyRevision
			var err error
			added := event
			added.AttemptID = fmt.Sprintf("%032x", 801)
			switch kind {
			case "ingest":
				err = s.WriteBatch(ctx, []model.Event{added})
			case "import":
				_, err = s.WriteImportBatch(ctx, []model.Event{added}, "cache-new")
			case "patch":
				err = s.WriteBatchWithPatches(ctx, nil, []model.ProxyResponsePatch{{ProxyRequestID: event.ProxyRequestID, StatusCode: 503}})
			case "pricing":
				_, err = s.UpdatePriceBook(ctx, aggregate.PriceBook{})
			case "pricing-provenance":
				err = s.ReplacePricingRules(ctx, fixturePriceBook().Rules, PricingProvenance{Source: "fixture", SourceDigest: fmt.Sprintf("%064x", 1), SyncedAt: s.now()})
			case "bindings":
				s.SetCatalogBindings(nil)
			case "reprice":
				_, err = s.Reprice(ctx, RepriceOptions{Range: model.Range{Start: q.Start, End: q.End, TimeZone: q.TimeZone}, ChunkSize: 1}, nil)
			case "retention":
				_, err = s.ApplyRetention(ctx, q.End, 100)
			case "purge":
				_, err = s.PurgeByKeyID(ctx, event.KeyID)
			case "rollback":
				_, err = s.RollbackImport(ctx, "cache-import")
			case "restore":
				err = s.Restore(ctx, backup, backup+".manifest.json")
			case "epoch":
				_, err = s.StartNewIdentityEpoch(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.historyRevision == before {
				t.Fatal("mutation did not advance revision")
			}
			afterMutation := count.Load()
			got, err := s.Summary(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			if count.Load() == afterMutation {
				t.Fatal("mutation reused stale result")
			}
			want, err := s.summaryUncached(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("cache parity after %s", kind)
			}
		})
	}
}

func TestHistoryCacheRejectsRacingPublicationAndPartialAnalysis(t *testing.T) {
	s, q, event := cacheFixture(t)
	ctx := context.Background()
	// Interleave a real late mutation after computing the result but before publish.
	_, err := cachedHistory(s, ctx, q, func() (value backend.HistoryCacheValue, err error) {
		value.Operation = model.OperationSummary
		value.Summary, err = s.summaryUncached(ctx, q)
		if err != nil {
			return
		}
		event.AttemptID = fmt.Sprintf("%032x", 802)
		err = s.WriteBatch(ctx, []model.Event{event})
		return
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.historyCache.Stats(); n != 0 {
		t.Fatal("racing result was cached")
	}
	q.Operation = model.OperationAnalysis
	_, err = cachedHistory(s, ctx, q, func() (backend.HistoryCacheValue, error) {
		return backend.HistoryCacheValue{Operation: q.Operation, Analysis: model.Analysis{}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.historyCache.Stats(); n != 0 {
		t.Fatal("partial analysis was cached")
	}
}

func TestHistoryCacheLateRecoveryInvalidates(t *testing.T) {
	t.Parallel()
	s, q, event := cacheFixture(t)
	ctx, count := historyQueryCounter()
	if _, err := s.ApplyRetention(ctx, q.End, 100); err != nil {
		t.Fatal(err)
	}
	event.AttemptID = fmt.Sprintf("%032x", 900)
	stop := errors.New("interrupt publication")
	interrupted := context.WithValue(ctx, c4bStepKey{}, func(name string) error {
		if name == "late:staged" {
			return stop
		}
		return nil
	})
	if _, err := s.WriteImportBatch(interrupted, []model.Event{event}, "late-cache"); !errors.Is(err, stop) {
		t.Fatalf("interruption: %v", err)
	}
	baseline, err := s.Summary(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	beforePending := count.Load()
	if _, err := s.Summary(ctx, q); err != nil {
		t.Fatal(err)
	}
	if count.Load() == beforePending {
		t.Fatal("pending publication was cached")
	}
	// Even an unrelated write resumes and publishes the older pending generation.
	tail := event
	tail.AttemptID = fmt.Sprintf("%032x", 901)
	tail.RequestedAt = s.now()
	if err := s.WriteBatch(ctx, []model.Event{tail}); err != nil {
		t.Fatal(err)
	}
	before := count.Load()
	got, err := s.Summary(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpstreamAttempts != baseline.UpstreamAttempts+1 || count.Load() == before {
		t.Fatalf("recovery did not refresh: before=%d after=%d", baseline.UpstreamAttempts, got.UpstreamAttempts)
	}
}

func TestHistoryCacheWaitsForVisibleWrites(t *testing.T) {
	t.Parallel()
	s, q, event := cacheFixture(t)
	ctx, count := historyQueryCounter()
	if _, err := s.Summary(ctx, q); err != nil {
		t.Fatal(err)
	}
	// Simulate a DSN/session configured to acknowledge before async flush. The
	// analytics writer must override it before returning and publishing a cache key.
	async := ch.Context(ctx, ch.WithSettings(ch.Settings{"async_insert": 1, "wait_for_async_insert": 0, "async_insert_busy_timeout_ms": 60000}))
	event.AttemptID = fmt.Sprintf("%032x", 950)
	if err := s.WriteBatch(async, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	before := count.Load()
	got, err := s.Summary(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpstreamAttempts != 2 || count.Load() == before {
		t.Fatalf("write not visible: attempts=%d", got.UpstreamAttempts)
	}
}

func TestHistoryCacheLegacyPublication(t *testing.T) {
	t.Parallel()
	cfg, _ := c4bLegacyFixture(t, true)
	ctx, count := historyQueryCounter()
	s, err := Open(ctx, chtest.Handle(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return now }
	q := model.Query{SchemaVersion: 2, Operation: model.OperationSummary, Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), TimeZone: "UTC"}
	empty, err := s.Summary(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	checked := false
	importing := context.WithValue(ctx, c4bStepKey{}, func(step string) error {
		if step != "legacy:yield" || checked {
			return nil
		}
		checked = true
		if !s.historyImporting {
			t.Fatal("import not marked mutable")
		}
		if _, err := s.Summary(ctx, q); err != nil {
			return err
		}
		before := count.Load()
		if _, err := s.Summary(ctx, q); err != nil {
			return err
		}
		if count.Load() == before {
			t.Fatal("partial import result was cached")
		}
		return nil
	})
	if err := RunLegacyImport(importing, s, cfg.Path); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("import did not yield")
	}
	before := count.Load()
	got, err := s.Summary(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpstreamAttempts <= empty.UpstreamAttempts || count.Load() == before {
		t.Fatal("legacy publication reused baseline")
	}
	before = count.Load()
	if _, err := s.Summary(ctx, q); err != nil {
		t.Fatal(err)
	}
	if count.Load() != before {
		t.Fatal("completed import not cacheable")
	}
}

func TestHistoryCacheIgnoresReplay(t *testing.T) {
	t.Parallel()
	s, q, event := cacheFixture(t)
	ctx, count := historyQueryCounter()
	if _, err := s.Summary(ctx, q); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	before := count.Load()
	if _, err := s.Summary(ctx, q); err != nil {
		t.Fatal(err)
	}
	if count.Load() != before {
		t.Fatal("duplicate event invalidated history")
	}
	patch := model.ProxyResponsePatch{ProxyRequestID: event.ProxyRequestID, StatusCode: 200}
	if err := s.WriteBatchWithPatches(ctx, nil, []model.ProxyResponsePatch{patch}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Summary(ctx, q); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteBatchWithPatches(ctx, nil, []model.ProxyResponsePatch{patch}); err != nil {
		t.Fatal(err)
	}
	before = count.Load()
	if _, err := s.Summary(ctx, q); err != nil {
		t.Fatal(err)
	}
	if count.Load() != before {
		t.Fatal("duplicate patch invalidated history")
	}
}
