package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func TestRetainedRollupsUseZoneLocalDayBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		zone      string
		localDate time.Time
		events    []time.Time
	}{
		{
			name:      "fractional offset",
			zone:      "Asia/Kolkata",
			localDate: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
			events: []time.Time{
				time.Date(2026, 8, 11, 18, 40, 0, 0, time.UTC),
				time.Date(2026, 8, 12, 18, 20, 0, 0, time.UTC),
			},
		},
		{
			name:      "DST fallback",
			zone:      "America/St_Johns",
			localDate: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			events: []time.Time{
				time.Date(2026, 11, 1, 2, 40, 0, 0, time.UTC),
				time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC),
				time.Date(2026, 11, 2, 3, 20, 0, 0, time.UTC),
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			location, err := time.LoadLocation(testCase.zone)
			if err != nil {
				t.Fatal(err)
			}
			localStart := time.Date(testCase.localDate.Year(), testCase.localDate.Month(), testCase.localDate.Day(), 0, 0, 0, 0, location)
			query := model.Query{SchemaVersion: 1, Start: localStart.UTC(), End: localStart.AddDate(0, 0, 1).UTC(), TimeZone: testCase.zone}
			database := openRetainedCorrectnessStore(t)
			events := retainedCorrectnessEvents(t, testCase.events)
			if err := database.WriteBatch(context.Background(), events); err != nil {
				t.Fatal(err)
			}
			wantHourly := retainedSnapshot(t, database, query, "1h")
			wantDaily := retainedSnapshot(t, database, query, "1d")
			if len(wantHourly.timeseries.Points) == 0 || !wantHourly.timeseries.Points[0].Start.Equal(localStart.UTC()) {
				t.Fatalf("first hourly bucket starts at %v, want local-hour boundary %v", wantHourly.timeseries.Points[0].Start, localStart.UTC())
			}
			retentionCutoff := query.End.Add(time.Hour)
			result, err := database.ApplyRetentionPolicy(context.Background(), retentionCutoff, time.Time{}, 100)
			if err != nil {
				t.Fatal(err)
			}
			if result.DeletedRows != int64(len(events)) {
				t.Fatalf("retention result = %+v", result)
			}
			assertRetainedSnapshot(t, database, query, "1h", wantHourly)
			result, err = database.ApplyRetentionPolicy(context.Background(), retentionCutoff, retentionCutoff, 100)
			if err != nil {
				t.Fatal(err)
			}
			if result.DeletedHourlyRows == 0 {
				t.Fatalf("daily retention result = %+v", result)
			}
			assertRetainedSnapshot(t, database, query, "1d", wantDaily)
		})
	}
}

func TestRetainedRangeValidationScopesKeyAndDimensionPredicates(t *testing.T) {
	database := openRetainedCorrectnessStore(t)
	events := retainedCorrectnessEvents(t, []time.Time{
		time.Date(2026, 8, 12, 4, 15, 0, 0, time.UTC),
		time.Date(2026, 8, 12, 5, 15, 0, 0, time.UTC),
	})
	events[0].Provider = "provider-retained"
	events[1].Provider = "provider-active"
	if err := database.WriteBatch(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ApplyRetention(context.Background(), time.Date(2026, 8, 12, 5, 0, 0, 0, time.UTC), 100); err != nil {
		t.Fatal(err)
	}

	query := model.Query{
		SchemaVersion: 1,
		Operation:     model.OperationSummary,
		Start:         time.Date(2026, 8, 12, 4, 30, 0, 0, time.UTC),
		End:           time.Date(2026, 8, 12, 6, 0, 0, 0, time.UTC),
		TimeZone:      "UTC",
		KeyIDs:        []string{events[1].KeyID},
	}
	summary, err := database.Summary(context.Background(), query)
	if err != nil || summary.UpstreamAttempts != 1 {
		t.Fatalf("key-scoped summary=%+v err=%v", summary, err)
	}

	query.KeyIDs = nil
	query.Filters = map[string]json.RawMessage{"provider": json.RawMessage(`["provider-active"]`)}
	summary, err = database.Summary(context.Background(), query)
	if err != nil || summary.UpstreamAttempts != 1 {
		t.Fatalf("dimension-scoped summary=%+v err=%v", summary, err)
	}

	query.Filters = map[string]json.RawMessage{"provider": json.RawMessage(`["provider-retained"]`)}
	if _, err := database.Summary(context.Background(), query); !errors.Is(err, ErrRetainedRangePartial) {
		t.Fatalf("matching partial rollup error=%v", err)
	}
}

func TestRetainedCacheDimensionUsesCacheReadTokens(t *testing.T) {
	database := openRetainedCorrectnessStore(t)
	events := retainedCorrectnessEvents(t, []time.Time{
		time.Date(2026, 8, 12, 4, 15, 0, 0, time.UTC),
		time.Date(2026, 8, 12, 5, 15, 0, 0, time.UTC),
		time.Date(2026, 8, 12, 6, 15, 0, 0, time.UTC),
	})
	events[0].Tokens.Cached, events[0].Tokens.CacheRead, events[0].Tokens.Total = 40, 0, 101
	events[1].Tokens.Cached, events[1].Tokens.CacheRead, events[1].Tokens.Total = 60, 0, 103
	events[2].Tokens.Cached, events[2].Tokens.CacheRead, events[2].Tokens.Total = 0, 25, 211
	if err := database.WriteBatch(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	query := model.Query{
		SchemaVersion: 1,
		Operation:     model.OperationDimensions,
		Dimension:     "cache",
		Start:         time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
		End:           time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
		TimeZone:      "UTC",
		PageSize:      10,
	}
	want, err := database.Dimensions(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(want.Rows) != 2 {
		t.Fatalf("raw cache dimensions = %+v", want.Rows)
	}

	result, err := database.ApplyRetentionPolicy(context.Background(), query.End, time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedRows != int64(len(events)) {
		t.Fatalf("hourly retention result = %+v", result)
	}
	assertCacheDimensionsEqual(t, database, query, want)

	result, err = database.ApplyRetentionPolicy(context.Background(), query.End, query.End, 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedHourlyRows == 0 {
		t.Fatalf("daily retention result = %+v", result)
	}
	assertCacheDimensionsEqual(t, database, query, want)
}

func assertCacheDimensionsEqual(t *testing.T, database *SQLiteStore, query model.Query, want model.DimensionPage) {
	t.Helper()
	got, err := database.Dimensions(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Rows, want.Rows) {
		t.Fatalf("retained cache dimensions\n got: %+v\nwant: %+v", got.Rows, want.Rows)
	}
}

func TestKeyCatalogSeparatesRangeAndLifetimeActivity(t *testing.T) {
	database := openRetainedCorrectnessStore(t)
	timestamps := []time.Time{
		time.Date(2026, 8, 10, 4, 15, 0, 0, time.UTC),
		time.Date(2026, 8, 12, 5, 15, 0, 0, time.UTC),
		time.Date(2026, 8, 14, 6, 15, 0, 0, time.UTC),
	}
	events := retainedCorrectnessEvents(t, timestamps)
	for index := range events {
		events[index].KeyID = events[0].KeyID
	}
	if err := database.WriteBatch(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ApplyRetention(context.Background(), time.Date(2026, 8, 10, 5, 0, 0, 0, time.UTC), 100); err != nil {
		t.Fatal(err)
	}
	query := model.Query{
		SchemaVersion: 1,
		Operation:     model.OperationDimensions,
		Dimension:     "key",
		Start:         time.Date(2026, 8, 12, 5, 0, 0, 0, time.UTC),
		End:           time.Date(2026, 8, 12, 6, 0, 0, 0, time.UTC),
		TimeZone:      "UTC",
		PageSize:      10,
	}
	page, err := database.KeyCatalog(context.Background(), query)
	if err != nil || len(page.Keys) != 1 {
		t.Fatalf("key catalog=%+v err=%v", page, err)
	}
	item := page.Keys[0]
	if item.FirstActivityAt == nil || !item.FirstActivityAt.Equal(timestamps[1]) ||
		item.LastActivityAt == nil || !item.LastActivityAt.Equal(timestamps[1]) {
		t.Fatalf("range activity = %v..%v", item.FirstActivityAt, item.LastActivityAt)
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	assertJSONTime(t, fields, "lifetime_first_activity_at", timestamps[0])
	assertJSONTime(t, fields, "lifetime_last_activity_at", timestamps[2])
}

func assertJSONTime(t *testing.T, fields map[string]json.RawMessage, name string, want time.Time) {
	t.Helper()
	raw, exists := fields[name]
	if !exists {
		t.Fatalf("key catalog is missing %s", name)
	}
	var got time.Time
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	if !got.Equal(want) {
		t.Fatalf("%s=%v want=%v", name, got, want)
	}
}

func openRetainedCorrectnessStore(t *testing.T) *SQLiteStore {
	t.Helper()
	database, err := Open(context.Background(), Config{
		Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20,
		PriceBook: fixturePriceBook(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	return database
}

func retainedCorrectnessEvents(t *testing.T, timestamps []time.Time) []model.Event {
	t.Helper()
	fixture := loadFixtureEvents(t)[0]
	events := make([]model.Event, len(timestamps))
	for index, timestamp := range timestamps {
		event := fixture
		event.AttemptID = fmt.Sprintf("%032x", index+1)
		event.ProxyRequestID = fmt.Sprintf("%032x", index+101)
		event.KeyID = fmt.Sprintf("%064x", index+1)
		event.RequestedAt = timestamp
		events[index] = event
	}
	return events
}
