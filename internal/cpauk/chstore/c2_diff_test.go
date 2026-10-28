package chstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	sqlstore "github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

type differentialReader struct {
	operation model.Operation
	ch        func(context.Context, *Store, model.Query) (any, error)
	sqlite    func(context.Context, *sqlstore.SQLiteStore, model.Query) (any, error)
}

// Add a reader in one line to run it against every compatible selection below.
var c2DifferentialReaders = []differentialReader{
	diffReader(model.OperationSummary, (*Store).Summary, (*sqlstore.SQLiteStore).Summary),
	diffReader(model.OperationTimeseries, (*Store).Timeseries, (*sqlstore.SQLiteStore).Timeseries),
	diffReader(model.OperationEvents, (*Store).Events, (*sqlstore.SQLiteStore).Events),
	diffReader(model.OperationActivity, (*Store).Activity, (*sqlstore.SQLiteStore).Activity),
	diffReader(model.OperationAnalysis, (*Store).Analysis, (*sqlstore.SQLiteStore).Analysis),
	diffReader(model.OperationModelLeaderboard, (*Store).ModelLeaderboard, (*sqlstore.SQLiteStore).ModelLeaderboard),
	diffReader(model.OperationModelCostSeries, (*Store).ModelCostSeries, (*sqlstore.SQLiteStore).ModelCostSeries),
	diffReader(model.OperationDimensions, (*Store).Dimensions, (*sqlstore.SQLiteStore).Dimensions),
	diffReader(model.OperationLeaderboard, (*Store).Leaderboard, (*sqlstore.SQLiteStore).Leaderboard),
}

func diffReader[T any](operation model.Operation, ch func(*Store, context.Context, model.Query) (T, error), sqlite func(*sqlstore.SQLiteStore, context.Context, model.Query) (T, error)) differentialReader {
	return differentialReader{operation, func(ctx context.Context, s *Store, q model.Query) (any, error) { return ch(s, ctx, q) }, func(ctx context.Context, s *sqlstore.SQLiteStore, q model.Query) (any, error) {
		return sqlite(s, ctx, q)
	}}
}

type differentialFixture struct {
	ch     *Store
	sqlite *sqlstore.SQLiteStore
	legacy *sql.DB
	events []model.Event
	codec  *model.CursorCodec
}

func openDifferentialFixture(t *testing.T, events []model.Event, capture int64, zones ...string) differentialFixture {
	t.Helper()
	ctx := context.Background()
	codec, err := model.NewCursorCodec([]byte("12345678901234567890123456789012"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: fixturePriceBook(), CursorCodec: codec}
	if len(zones) > 0 {
		cfg.RetentionTimeZone = zones[0]
	}
	ch := openTestStore(t, cfg)
	cfg.Path = filepath.Join(t.TempDir(), "legacy.db")
	sqlite, err := sqlstore.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlite.Close(ctx) })
	legacy, err := sql.Open("sqlite", cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	if _, err := legacy.Exec("UPDATE schema_migrations SET applied_at_ns=? WHERE version=18", capture); err != nil {
		t.Fatal(err)
	}
	if err := ch.setMetadata(ctx, "session_capture_since_ns", strconv.FormatInt(capture, 10)); err != nil {
		t.Fatal(err)
	}
	// Both native intake and imported ownership must survive every read.
	var native, imported []model.Event
	for i, e := range events {
		if i%7 == 0 {
			imported = append(imported, e)
		} else {
			native = append(native, e)
		}
	}
	if err := ch.WriteBatch(ctx, native); err != nil {
		t.Fatal(err)
	}
	if err := sqlite.WriteBatch(ctx, native); err != nil {
		t.Fatal(err)
	}
	if len(imported) > 0 {
		if _, err := ch.WriteImportBatch(ctx, imported, "diff-import"); err != nil {
			t.Fatal(err)
		}
		if _, err := sqlite.WriteImportBatch(ctx, imported, "diff-import"); err != nil {
			t.Fatal(err)
		}
	}
	var patches []model.ProxyResponsePatch
	for i, e := range events {
		if i%5 == 0 {
			patches = append(patches, model.ProxyResponsePatch{ProxyRequestID: e.ProxyRequestID, StatusCode: 502, Error: "first", RespondedAt: e.RequestedAt.Add(time.Second)}, model.ProxyResponsePatch{ProxyRequestID: e.ProxyRequestID, StatusCode: 200, RespondedAt: e.RequestedAt.Add(2 * time.Second)})
		}
	}
	if err := ch.WriteBatchWithPatches(ctx, nil, patches); err != nil {
		t.Fatal(err)
	}
	if err := sqlite.WriteBatchWithPatches(ctx, nil, patches); err != nil {
		t.Fatal(err)
	}
	return differentialFixture{ch, sqlite, legacy, events, codec}
}

// Synthetic attempts span the US spring DST transition, retries, explicit and
// historical cancellation, incomplete timing, session sources and token quality.
func syntheticDifferentialEvents(t testing.TB) []model.Event {
	t.Helper()
	base := loadFixtureEvents(t)[0]
	start := time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC)
	events := make([]model.Event, 96)
	for i := range events {
		e := base
		e.AttemptID = fmt.Sprintf("%032x", i+1000)
		e.ProxyRequestID = fmt.Sprintf("%032x", i/3+2000)
		e.KeyID = model.KeyID(fmt.Sprint(i % 3))
		e.RequestedAt = start.Add(time.Duration(i) * 77 * time.Minute)
		e.Provider = []string{"provider-a", "provider-b"}[i%2]
		e.Model = base.Model
		e.CredentialID = nil
		e.CredentialIDAlgorithm = nil
		if i%3 == 1 {
			e.CredentialID = base.CredentialID
			e.CredentialIDAlgorithm = base.CredentialIDAlgorithm
		}
		e.AuthType = nil
		e.ServiceTierUsed = nil
		e.ServiceTierRequested = nil
		if i%3 == 1 {
			tier := "priority"
			e.ServiceTierUsed = &tier
		} else if i%3 == 2 {
			tier := "default"
			e.ServiceTierRequested = &tier
		}
		if i%3 == 2 {
			auth := "oauth"
			e.AuthType = &auth
		}
		e.ProxyStatusCode = nil
		e.ProxyError = nil
		e.RespondedAt = nil
		e.UpstreamErrorBody = nil
		e.UpstreamStatusCode = nil
		e.FailureHop = nil
		e.CancellationSource = nil
		e.SessionID = nil
		e.SessionSource = nil
		e.Outcome = ""
		e.Succeeded = i%4 != 0
		e.Tokens.Quality = []model.TokenQuality{model.TokenQualityExact, model.TokenQualityEstimated, model.TokenQualityMissing}[i%3]
		if i%4 == 1 {
			v := fmt.Sprint("session-", i%3)
			source := []string{"claude", "codex", "header"}[i%3]
			e.SessionID = &v
			e.SessionSource = &source
		}
		if i%11 == 0 {
			id := "codex:prefixed"
			e.SessionID = &id
			e.SessionSource = nil
		}
		if i%13 == 0 {
			id := "derived:internal"
			e.SessionID = &id
			e.SessionSource = nil
		}
		if i%6 == 0 {
			e.Outcome = "canceled"
			e.Succeeded = true
		}
		if i%6 == 1 {
			e.Outcome = "succeeded"
		}
		if i%6 == 2 {
			e.Succeeded = false
			body := "Post \"https://example.test\": context canceled"
			e.UpstreamErrorBody = &body
		}
		if i%8 == 0 {
			status := 503
			e.UpstreamStatusCode = &status
			errorClass := "upstream"
			e.ErrorClass = &errorClass
		} else {
			e.ErrorClass = nil
		}
		if i%9 == 0 {
			status := 201
			at := e.RequestedAt.Add(3 * time.Second)
			e.ProxyStatusCode = &status
			e.RespondedAt = &at
		}
		if i%3 == 0 {
			v := int64(i + 1)
			e.GenerationTimeMS = &v
			e.TimeToFirstTokenMS = &v
			e.FirstTokenLatencyMS = &v
			e.ProviderLatencyMS = &v
		} else {
			e.GenerationTimeMS = nil
			e.TimeToFirstTokenMS = nil
			e.FirstTokenLatencyMS = nil
			e.ProviderLatencyMS = nil
		}
		events[i] = e
	}
	return events
}

func (f differentialFixture) compare(t *testing.T, reader differentialReader, q model.Query) {
	t.Helper()
	ctx := context.Background()
	q.Operation = reader.operation
	switch q.Operation {
	case model.OperationEvents:
		q.PageSize = 13
		q.BucketWidth = ""
	case model.OperationDimensions:
		q.PageSize = 13
		q.BucketWidth = ""
		if q.Dimension == "" {
			q.Dimension = "provider"
		}
	case model.OperationLeaderboard:
		q.PageSize = 13
		q.BucketWidth = ""
		if q.SortBy == "" {
			q.SortBy = model.LeaderboardSortTokens
		}
	case model.OperationTimeseries:
		if q.BucketWidth == "" {
			q.BucketWidth = "1d"
		}
	case model.OperationSummary, model.OperationModelLeaderboard:
		q.BucketWidth = ""
	case model.OperationActivity:
		q.BucketWidth = ""
		if q.Window == "" {
			q.Window = "year"
		}
	case model.OperationModelCostSeries:
		if q.BucketWidth == "" {
			q.BucketWidth = "1d"
		}
		if _, ok := q.Filters["model"]; !ok {
			q.Filters = cloneDiffFilters(q.Filters)
			value, _ := json.Marshal([]string{f.events[0].Model})
			q.Filters["model"] = value
		}
	}
	cq, sq := q, q
	for page := 0; page < 100; page++ {
		got, e1 := reader.ch(ctx, f.ch, cq)
		want, e2 := reader.sqlite(ctx, f.sqlite, sq)
		if e1 != nil || e2 != nil {
			if e1 == nil || e2 == nil || e1.Error() != e2.Error() {
				t.Fatalf("errors differ: CH %v SQLite %v", e1, e2)
			}
			return
		}
		var nextCH, nextSQL string
		got, nextCH = normalizeDiffPage(got)
		want, nextSQL = normalizeDiffPage(want)
		if (nextCH == "") != (nextSQL == "") {
			t.Fatal("cursor presence differs")
		}
		if nextCH != "" {
			ca, e1 := f.codec.Decode(nextCH)
			cb, e2 := f.codec.Decode(nextSQL)
			if e1 != nil || e2 != nil || !reflect.DeepEqual(ca, cb) {
				t.Fatalf("cursor differs: %+v %+v", ca, cb)
			}
		}
		if !reflect.DeepEqual(got, want) {
			a, _ := json.MarshalIndent(got, "", "  ")
			b, _ := json.MarshalIndent(want, "", "  ")
			t.Fatalf("results differ\nCH %s\nSQLite %s", a, b)
		}
		if nextCH == "" {
			return
		}
		cq.Cursor, sq.Cursor = nextCH, nextSQL
	}
	t.Fatal("pagination did not terminate")
}

func cloneDiffFilters(filters map[string]json.RawMessage) map[string]json.RawMessage {
	copy := make(map[string]json.RawMessage, len(filters)+1)
	for k, v := range filters {
		copy[k] = v
	}
	return copy
}
func normalizeDiffPage(value any) (any, string) {
	switch page := value.(type) {
	case model.EventPage:
		next := page.Meta.NextCursor
		page.Meta.NextCursor = ""
		return page, next
	case model.DimensionPage:
		next := page.Meta.NextCursor
		page.Meta.NextCursor = ""
		return page, next
	case model.LeaderboardPage:
		next := page.Meta.NextCursor
		page.Meta.NextCursor = ""
		return page, next
	default:
		return value, ""
	}
}

func TestC2DifferentialRaw(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"fixture", "synthetic"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			events := loadFixtureEvents(t)
			if kind == "synthetic" {
				events = syntheticDifferentialEvents(t)
			}
			f := openDifferentialFixture(t, events, events[0].RequestedAt.Add(24*time.Hour).UnixNano())
			start, end := events[0].RequestedAt, events[0].RequestedAt
			for _, e := range events {
				if e.RequestedAt.Before(start) {
					start = e.RequestedAt
				}
				if e.RequestedAt.After(end) {
					end = e.RequestedAt
				}
			}
			base := model.Query{SchemaVersion: 2, Start: start.Add(-time.Hour), End: end.Add(time.Hour), TimeZone: "UTC"}
			filters := []struct{ name, value string }{{"", ""}, {"provider", `["` + events[0].Provider + `"]`}, {"model", `["` + events[0].Model + `"]`}, {"success", "true"}, {"success", "false"}, {"generated", "true"}, {"result", `"failure"`}, {"outcome", `"canceled"`}, {"status_code", "[503]"}, {"token_quality", `["exact"]`}, {"session_id", `["session-1"]`}, {"session_id", `["codex:session-1"]`}, {"session_id", `["prefixed"]`}, {"session_id", `["derived:internal"]`}, {"source", `["native"]`}, {"source", `["diff-import"]`}, {"credential_id", `["missing"]`}, {"auth_type", `["oauth"]`}, {"endpoint_class", `["responses"]`}, {"service_tier", `["priority"]`}, {"error_class", `["upstream"]`}}
			for _, reader := range c2DifferentialReaders {
				for i, filter := range filters {
					// Exhaustive filter semantics use the richer synthetic fixture.
					// The legacy fixture checks its distinct encoding/price shape.
					if kind == "fixture" && i > 1 && filter.name != "source" {
						continue
					}
					t.Run(fmt.Sprintf("%s/filter-%d-%s", reader.operation, i, filter.name), func(t *testing.T) {
						q := base
						if filter.name != "" {
							q.Filters = map[string]json.RawMessage{filter.name: json.RawMessage(filter.value)}
						}
						f.compare(t, reader, q)
					})
				}
				for _, zone := range []string{"UTC", "America/New_York", "Asia/Kathmandu", "Australia/Lord_Howe"} {
					if kind == "fixture" && zone != "UTC" {
						continue
					}
					for _, width := range []string{"1m", "5m", "15m", "30m", "1h", "1d", "1w"} {
						if reader.operation != model.OperationTimeseries && width != "1d" {
							continue
						}
						t.Run(fmt.Sprint(reader.operation, "/", zone, "/", width), func(t *testing.T) { q := base; q.TimeZone = zone; q.BucketWidth = width; f.compare(t, reader, q) })
					}
				}
				for _, variant := range []string{"key", "empty", "partial"} {
					t.Run(fmt.Sprint(reader.operation, "/", variant), func(t *testing.T) {
						q := base
						switch variant {
						case "key":
							q.KeyIDs = []string{events[0].KeyID}
						case "empty":
							q.Start = end.Add(time.Hour)
							q.End = q.Start.Add(time.Hour)
						case "partial":
							q.Start = start.Add(17 * time.Minute)
							q.End = end.Add(-17 * time.Minute)
						}
						f.compare(t, reader, q)
					})
				}
			}
		})
	}
}

// copyRetained imports SQLite's real retained facts rather than reconstructing
// retention in this test. C4's writer is tested separately by its own slice.
func (f differentialFixture) copyRetained(t *testing.T, raw, hourly time.Time) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{"rollups", "request_rollups"} {
		rows, err := f.legacy.Query("SELECT * FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		batch, err := f.ch.db.Conn().PrepareBatch(ctx, "INSERT INTO "+f.ch.table("analytics_"+table)+" ("+strings.Join(columns, ",")+",version,is_deleted)")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			args := make([]any, len(columns))
			for i := range values {
				args[i] = &values[i]
			}
			if err := rows.Scan(args...); err != nil {
				t.Fatal(err)
			}
			values = append(values, f.ch.nextVersion(), uint8(0))
			if err := batch.Append(values...); err != nil {
				t.Fatal(err)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		if err := batch.Send(); err != nil {
			t.Fatal(err)
		}
	}
	metadata := map[string]string{"retention_raw_cutoff_ns": strconv.FormatInt(raw.UnixNano(), 10), "retention_time_zone": f.ch.config.RetentionTimeZone}
	if !hourly.IsZero() {
		metadata["retention_hourly_cutoff_ns"] = strconv.FormatInt(hourly.UnixNano(), 10)
	}
	if err := f.ch.setMetadataBatch(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	f.ch.retentionCutoff = raw
}
func TestC2DifferentialRetained(t *testing.T) {
	t.Parallel()
	for _, zone := range []string{"UTC", "America/New_York", "Asia/Kathmandu"} {
		for _, daily := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/daily-%t", zone, daily), func(t *testing.T) {
				t.Parallel()
				events := syntheticDifferentialEvents(t)
				f := openDifferentialFixture(t, events, events[0].RequestedAt.Add(time.Hour).UnixNano(), zone)
				location, err := time.LoadLocation(zone)
				if err != nil {
					t.Fatal(err)
				}
				start, _, err := aggregate.BucketBounds(events[0].RequestedAt, zone, "1d")
				if err != nil {
					t.Fatal(err)
				}
				_, end, err := aggregate.BucketBounds(events[len(events)-1].RequestedAt, zone, "1d")
				if err != nil {
					t.Fatal(err)
				}
				raw := start.In(location).AddDate(0, 0, 3).UTC()
				hourly := time.Time{}
				if daily {
					hourly = start.In(location).AddDate(0, 0, 2).UTC()
				}
				if _, err := f.sqlite.ApplyRetentionPolicy(context.Background(), raw, hourly, 1000); err != nil {
					t.Fatal(err)
				}
				f.copyRetained(t, raw, hourly)
				base := model.Query{SchemaVersion: 2, Start: start, End: end, TimeZone: zone}
				for _, reader := range c2DifferentialReaders {
					for _, variant := range []string{"all", "key", "provider", "success", "partial", "timezone", "narrow", "raw-only", "unsupported"} {
						t.Run(fmt.Sprint(reader.operation, "/", variant), func(t *testing.T) {
							q := base
							switch variant {
							case "key":
								q.KeyIDs = []string{events[0].KeyID}
							case "provider":
								q.Filters = map[string]json.RawMessage{"provider": json.RawMessage(`["provider-a"]`)}
							case "success":
								q.Filters = map[string]json.RawMessage{"success": json.RawMessage(`true`)}
							case "partial":
								q.Start = q.Start.Add(time.Minute)
							case "timezone":
								q.TimeZone = "Australia/Lord_Howe"
							case "narrow":
								q.BucketWidth = "15m"
							case "raw-only":
								q.Start = raw
							case "unsupported":
								q.Filters = map[string]json.RawMessage{"generated": json.RawMessage(`true`)}
							}
							f.compare(t, reader, q)
						})
					}
				}
			})
		}
	}
}

func TestC2DifferentialFractionalDST(t *testing.T) {
	t.Parallel()
	for _, start := range []time.Time{time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC), time.Date(2026, 4, 4, 13, 0, 0, 0, time.UTC)} {
		t.Run(start.Format("2006-01-02"), func(t *testing.T) {
			events := syntheticDifferentialEvents(t)[:24]
			for i := range events {
				events[i].RequestedAt = start.Add(time.Duration(i) * 10 * time.Minute)
			}
			f := openDifferentialFixture(t, events, 0)
			for _, width := range []string{"30m", "1h", "1d", "1w"} {
				t.Run(width, func(t *testing.T) {
					q := model.Query{SchemaVersion: 2, Start: start, End: start.Add(4 * time.Hour), TimeZone: "Australia/Lord_Howe", BucketWidth: width}
					f.compare(t, c2DifferentialReaders[1], q)
				})
			}
		})
	}
}
