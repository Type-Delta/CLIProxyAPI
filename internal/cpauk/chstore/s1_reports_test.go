package chstore

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func s1Fixture(t *testing.T, count int) (*Store, model.Query) {
	t.Helper()
	s := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 4 << 30, PriceBook: fixturePriceBook()})
	ctx := context.Background()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for base := 0; base < count; base += 5000 {
		events := make([]model.Event, min(5000, count-base))
		for j := range events {
			i := base + j
			// Four attempts share each request, including requests across retention.
			at := start.Add(time.Duration(i%4)*24*time.Hour + time.Duration(i/4)*time.Millisecond)
			credential := model.KeyID(fmt.Sprintf("credential-%d", i/4%5))
			algorithm := model.CredentialIDAlgorithm
			auth := []string{"api-key", "oauth"}[i%2]
			var class *string
			if i%3 == 0 {
				v := fmt.Sprintf("error-%d", i%5)
				class = &v
			}
			e := c3V2Event(fmt.Sprintf("%032x", i+1), fmt.Sprintf("%032x", i/4+1), fmt.Sprintf("%064x", i/4%12+1), at, i%3 != 0, class, 0, 0, 100, 20)
			e.CredentialID = &credential
			e.CredentialIDAlgorithm = &algorithm
			e.AuthType = &auth
			e.Model = []string{"model-f93b", "unpriced-a", "unpriced-b"}[i%3]
			generation := int64(i % 20)
			if i%2 == 0 {
				e.GenerationTimeMS = &generation
			}
			events[j] = e
		}
		if err := s.WriteBatch(ctx, events); err != nil {
			t.Fatal(err)
		}
	}
	// Deliberate equal timestamps: earliest attempt wins differing auth/error.
	ties := []model.Event{}
	for i := 0; i < 2; i++ {
		e := c3V2Event(fmt.Sprintf("%032x", count+i+1), fmt.Sprintf("%032x", count+i+1), fmt.Sprintf("%064x", 1), start.Add(3*24*time.Hour+time.Hour), false, c3PointerString(fmt.Sprintf("tie-%d", i)), 0, 0, 100, 20)
		e.CredentialID = c3PointerString(model.KeyID("credential-0"))
		e.CredentialIDAlgorithm = c3PointerString(model.CredentialIDAlgorithm)
		e.AuthType = c3PointerString(fmt.Sprintf("auth-%d", i))
		ties = append(ties, e)
	}
	if err := s.WriteBatch(ctx, ties); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateKeyLifecycle(ctx, []string{fmt.Sprintf("%064x", 1), fmt.Sprintf("%064x", 100)}, []string{fmt.Sprintf("%064x", 2)}); err != nil {
		t.Fatal(err)
	}
	q := model.Query{SchemaVersion: 1, Operation: model.OperationDimensions, Dimension: "key", PageSize: 5, Start: start, End: start.Add(5 * 24 * time.Hour), TimeZone: "UTC"}
	return s, q
}

func s1AssertEqual(t *testing.T, want, got any) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		a, _ := json.Marshal(want)
		b, _ := json.Marshal(got)
		t.Fatalf("legacy=%s\nnew=%s", a, b)
	}
}

func TestS1ReportParity(t *testing.T) {
	s, q := s1Fixture(t, 80)
	ctx := context.Background()
	// Outcome patches must have no effect on these attempt-based reports.
	if err := s.WriteBatchWithPatches(ctx, nil, []model.ProxyResponsePatch{{ProxyRequestID: fmt.Sprintf("%032x", 1), StatusCode: 503, RespondedAt: q.Start.Add(time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceProviderQuotaSnapshots(ctx, []ProviderQuotaSnapshot{{Provider: "provider-v2", CredentialID: model.KeyID("credential-0"), Model: "quota", Available: true, ObservedAt: q.End}, {Provider: "snapshot-only", CredentialID: model.KeyID("snapshot-only"), Available: false, Disabled: true, ObservedAt: q.End}}); err != nil {
		t.Fatal(err)
	}

	for _, retained := range []bool{false, true} {
		if retained {
			if _, err := s.ApplyRetention(ctx, q.Start.Add(2*24*time.Hour), 1000); err != nil {
				t.Fatal(err)
			}
		}
		for _, filtered := range []bool{false, true} {
			selected := q
			if filtered {
				selected.KeyIDs = []string{fmt.Sprintf("%064x", 1), fmt.Sprintf("%064x", 100)}
			}
			for {
				want, err := s.s1LegacyKeyCatalog(ctx, selected)
				if err != nil {
					t.Fatal(err)
				}
				got, err := s.KeyCatalog(ctx, selected)
				if err != nil {
					t.Fatal(err)
				}
				if want.Meta.NextCursor != "" {
					a, err := s.config.CursorCodec.Decode(want.Meta.NextCursor)
					if err != nil {
						t.Fatal(err)
					}
					b, err := s.config.CursorCodec.Decode(got.Meta.NextCursor)
					if err != nil {
						t.Fatal(err)
					}
					s1AssertEqual(t, a, b)
					want.Meta.NextCursor = got.Meta.NextCursor
				}
				s1AssertEqual(t, want, got)
				if got.Meta.NextCursor == "" {
					break
				}
				selected.Cursor = got.Meta.NextCursor
			}
		}
		want, err := s.s1LegacyProviderCredentials(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.ProviderCredentials(ctx)
		if err != nil {
			t.Fatal(err)
		}
		s1AssertEqual(t, want, got)
		r := model.Range{Start: q.Start, End: q.End, TimeZone: q.TimeZone}
		wantMissing, err := s.s1LegacyPricingMissing(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		gotMissing, err := s.PricingMissing(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		s1AssertEqual(t, wantMissing, gotMissing)
	}
}

// TestS1ReportBenchmark compares the preserved row-streaming readers with SQL
// aggregation on identical data. Each context tags its complete query tree.
func TestS1ReportBenchmark(t *testing.T) {
	if os.Getenv("CPA_S1_BENCH") != "1" {
		t.Skip("set CPA_S1_BENCH=1")
	}
	s, q := s1Fixture(t, 200000)
	ctx := context.Background()
	if _, err := s.ApplyRetention(ctx, q.Start.Add(2*24*time.Hour), 10000); err != nil {
		t.Fatal(err)
	}
	r := model.Range{Start: q.Start, End: q.End, TimeZone: q.TimeZone}
	readers := []struct {
		name string
		call func(context.Context) error
	}{
		{"keys_old", func(c context.Context) error { _, e := s.s1LegacyKeyCatalog(c, q); return e }},
		{"keys_new", func(c context.Context) error { _, e := s.KeyCatalog(c, q); return e }},
		{"providers_old", func(c context.Context) error { _, e := s.s1LegacyProviderCredentials(c); return e }},
		{"providers_new", func(c context.Context) error { _, e := s.ProviderCredentials(c); return e }},
		{"missing_old", func(c context.Context) error { _, e := s.s1LegacyPricingMissing(c, r); return e }},
		{"missing_new", func(c context.Context) error { _, e := s.PricingMissing(c, r); return e }},
	}
	ids := map[string]string{}
	for _, reader := range readers {
		id := fmt.Sprintf("s1_%s_%d", reader.name, time.Now().UnixNano())
		ids[reader.name] = id
		c := ch.Context(ctx, ch.WithQueryID(id))
		start := time.Now()
		if err := reader.call(c); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s wall=%s", reader.name, time.Since(start))
	}
	if err := s.db.Conn().Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatal(err)
	}
	for _, reader := range readers {
		var queries, read, bytes, result uint64
		err := s.db.Conn().QueryRow(ctx, "SELECT count(),sum(read_rows),sum(read_bytes),sum(result_rows) FROM system.query_log WHERE type='QueryFinish' AND query_id=? AND NOT startsWith(query,'INSERT')", ids[reader.name]).Scan(&queries, &read, &bytes, &result)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s queries=%d read_rows=%d read_bytes=%d result_rows=%d", reader.name, queries, read, bytes, result)
	}
}

func TestS1KeyCatalogMoneyOverflow(t *testing.T) {
	for _, sameModel := range []bool{true, false} {
		t.Run(fmt.Sprint(sameModel), func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			events := loadFixtureEvents(t)
			events = events[:2]
			events[1].KeyID = events[0].KeyID
			if sameModel {
				events[1].Model = events[0].Model
			} else {
				events[1].Model = "different"
			}
			huge, one := model.NanoUSD(math.MaxInt64), model.NanoUSD(1)
			events[0].KnownCost = &huge
			events[1].KnownCost = &one
			if _, err := s.insertEventsMode(ctx, events, "", true, true); err != nil {
				t.Fatal(err)
			}
			q := model.Query{SchemaVersion: 1, Operation: model.OperationDimensions, Dimension: "key", PageSize: 200, Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC), TimeZone: "UTC"}
			_, oldErr := s.s1LegacyKeyCatalog(ctx, q)
			_, newErr := s.KeyCatalog(ctx, q)
			if oldErr == nil || newErr == nil || oldErr.Error() != newErr.Error() {
				t.Fatalf("old=%v new=%v", oldErr, newErr)
			}
		})
	}
}

func TestS1ProviderRetainedTimestampTies(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	events := make([]model.Event, 4)
	for i := range events {
		e := c3V2Event(fmt.Sprintf("%032x", i+1), fmt.Sprintf("%032x", i/2+1), fmt.Sprintf("%064x", i+1), start.Add(time.Minute), false, c3PointerString(fmt.Sprintf("error-%d", 3-i)), 0, 0, 10, 20)
		e.CredentialID = c3PointerString(model.KeyID("tie-credential"))
		e.CredentialIDAlgorithm = c3PointerString(model.CredentialIDAlgorithm)
		e.AuthType = c3PointerString(fmt.Sprintf("auth-%d", 3-i))
		events[i] = e
	}
	if err := s.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	for _, retained := range []bool{false, true} {
		if retained {
			if _, err := s.ApplyRetention(ctx, start.Add(time.Hour), 100); err != nil {
				t.Fatal(err)
			}
		}
		want, err := s.s1LegacyProviderCredentials(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.ProviderCredentials(ctx)
		if err != nil {
			t.Fatal(err)
		}
		s1AssertEqual(t, want, got)
	}
}
