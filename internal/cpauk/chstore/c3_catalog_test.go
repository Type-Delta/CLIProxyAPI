package chstore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

func TestC3KeyCatalogMatchesSQLite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ch := openTestStore(t)
	sq, err := store.Open(ctx, store.Config{Path: filepath.Join(t.TempDir(), "a.db"), MaxStorageBytes: 64 << 20, PriceBook: fixturePriceBook()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sq.Close(ctx) }()
	events := loadFixtureEvents(t)
	extra := events[0]
	extra.AttemptID = strings.Repeat("e", 32)
	extra.ProxyRequestID = strings.Repeat("f", 32)
	extra.Model = "unpriced-model"
	extra.Tokens.Input = 5000
	extra.Tokens.Total = 5030
	events = append(events, extra)
	for _, db := range []interface {
		WriteBatch(context.Context, []model.Event) error
		UpdateKeyLifecycle(context.Context, []string, []string) error
	}{ch, sq} {
		if err := db.WriteBatch(ctx, events); err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateKeyLifecycle(ctx, []string{events[0].KeyID, events[1].KeyID}, nil); err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateKeyLifecycle(ctx, []string{events[2].KeyID, strings.Repeat("d", 64)}, []string{events[0].KeyID}); err != nil {
			t.Fatal(err)
		}
	}
	q := model.Query{SchemaVersion: 1, Operation: model.OperationDimensions, Dimension: "key", PageSize: 1, Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), TimeZone: "UTC"}
	collect := func(db interface {
		KeyCatalog(context.Context, model.Query) (KeyCatalogPage, error)
	}) map[string]model.KeyIdentity {
		out := map[string]model.KeyIdentity{}
		query := q
		for {
			page, err := db.KeyCatalog(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range page.Keys {
				out[item.KeyID] = item
			}
			if page.Meta.NextCursor == "" {
				return out
			}
			query.Cursor = page.Meta.NextCursor
		}
	}
	got, want := collect(ch), collect(sq)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ClickHouse %+v\nSQLite %+v", got, want)
	}
}

func c3InsertRetained(t *testing.T, s *Store, start time.Time, key, provider, credential, request string, failed bool) {
	t.Helper()
	ctx := context.Background()
	end := start.Add(time.Hour)
	succeeded := int64(1)
	class := ""
	if failed {
		succeeded = 0
		class = "quota"
	}
	version := s.nextVersion()
	sql := "INSERT INTO " + s.table("analytics_rollups") + " (grain,bucket_start_ns,bucket_end_ns,first_activity_ns,last_activity_ns,key_id,provider,model,credential_id,auth_type,succeeded,error_class,proxy_requests,upstream_attempts,total_tokens,unpriced_tokens,version,is_deleted) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"
	if err := s.db.Conn().Exec(ctx, sql, "hourly", start.UnixNano(), end.UnixNano(), start.Add(time.Minute).UnixNano(), start.Add(2*time.Minute).UnixNano(), key, provider, "test-model", credential, "oauth", succeeded, class, int64(1), int64(1), int64(20), int64(20), version, uint8(0)); err != nil {
		t.Fatal(err)
	}
	sql = "INSERT INTO " + s.table("analytics_request_rollups") + " (grain,bucket_start_ns,bucket_end_ns,proxy_request_id,key_id,provider,model,credential_id,auth_type,succeeded,error_class,version,is_deleted) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)"
	if err := s.db.Conn().Exec(ctx, sql, "hourly", start.UnixNano(), end.UnixNano(), request, key, provider, "test-model", credential, "oauth", succeeded, class, s.nextVersion(), uint8(0)); err != nil {
		t.Fatal(err)
	}
	if err := s.setMetadata(ctx, "retention_raw_cutoff_ns", fmt.Sprint(end.UnixNano())); err != nil {
		t.Fatal(err)
	}
	s.retentionCutoff = end
}
func TestC3RetainedProviderCredentialsAndKeyCatalog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	id := strings.Repeat("a", 64)
	key := strings.Repeat("b", 64)
	request := strings.Repeat("c", 32)
	c3InsertRetained(t, s, start, key, "codex", id, request, true)
	credentials, err := s.ProviderCredentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(credentials) != 1 || credentials[0].Requests != 1 || credentials[0].Failed != 1 || credentials[0].AuthType != "oauth" || credentials[0].LastErrorClass == nil || *credentials[0].LastErrorClass != "quota" {
		t.Fatalf("credentials %+v", credentials)
	}
	q := model.Query{SchemaVersion: 1, Operation: model.OperationDimensions, Dimension: "key", PageSize: 10, Start: start, End: start.Add(time.Hour), TimeZone: "UTC"}
	page, err := s.KeyCatalog(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Keys) != 1 || page.Keys[0].TotalTokens != 20 || page.Keys[0].Requests != 1 || page.Keys[0].FirstActivityAt == nil || page.Keys[0].LifetimeLastActivityAt == nil {
		t.Fatalf("catalog %+v", page)
	}
}

func TestC3PricingMissingMixedRetainedMatchesSQLite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	sq, err := store.Open(ctx, store.Config{Path: filepath.Join(t.TempDir(), "a.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sq.Close(ctx) }()
	first := loadFixtureEvents(t)[0]
	first.RequestedAt = start.Add(time.Minute)
	first.AttemptID = strings.Repeat("1", 32)
	first.ProxyRequestID = strings.Repeat("2", 32)
	second := first
	second.RequestedAt = start.Add(2 * time.Minute)
	second.AttemptID = strings.Repeat("3", 32)
	second.ProxyRequestID = strings.Repeat("4", 32)
	if err := sq.WriteBatch(ctx, []model.Event{first}); err != nil {
		t.Fatal(err)
	}
	if _, err := sq.UpdatePriceBook(ctx, fixturePriceBook()); err != nil {
		t.Fatal(err)
	}
	if err := sq.WriteBatch(ctx, []model.Event{second}); err != nil {
		t.Fatal(err)
	}
	if _, err := sq.ApplyRetention(ctx, start.Add(time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	selected := model.Range{Start: start, End: start.Add(time.Hour), TimeZone: "UTC"}
	want, err := sq.PricingMissing(ctx, selected)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 1 || want[0].Requests != 2 {
		t.Fatalf("SQLite mixed bucket %+v", want)
	}
	ch := openTestStore(t)
	end := start.Add(time.Hour)
	sql := "INSERT INTO " + ch.table("analytics_rollups") + " (grain,bucket_start_ns,bucket_end_ns,first_activity_ns,last_activity_ns,key_id,provider,model,succeeded,proxy_requests,upstream_attempts,total_tokens,unpriced_tokens,version,is_deleted) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"
	if err := ch.db.Conn().Exec(ctx, sql, "hourly", start.UnixNano(), end.UnixNano(), first.RequestedAt.UnixNano(), second.RequestedAt.UnixNano(), first.KeyID, first.Provider, first.Model, int64(1), int64(2), int64(2), first.Tokens.Total+second.Tokens.Total, first.Tokens.Total, ch.nextVersion(), uint8(0)); err != nil {
		t.Fatal(err)
	}
	if err := ch.setMetadata(ctx, "retention_raw_cutoff_ns", fmt.Sprint(end.UnixNano())); err != nil {
		t.Fatal(err)
	}
	ch.retentionCutoff = end
	got, err := ch.PricingMissing(ctx, selected)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mixed pricing CH=%+v SQLite=%+v", got, want)
	}
}

func TestC3RepriceIgnoresPublishedRawSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	event := loadFixtureEvents(t)[0]
	if err := s.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	cutoff := event.RequestedAt.Add(time.Nanosecond)
	if err := s.setMetadata(ctx, "retention_raw_cutoff_ns", fmt.Sprint(cutoff.UnixNano())); err != nil {
		t.Fatal(err)
	}
	s.retentionCutoff = cutoff
	result, err := s.Reprice(ctx, RepriceOptions{Range: model.Range{Start: event.RequestedAt.Add(-time.Minute), End: event.RequestedAt.Add(time.Minute), TimeZone: "UTC"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.HistoryComplete || result.Matched != 0 || result.Updated != 0 || result.RetainedCutoff == nil || !result.RetainedCutoff.Equal(cutoff) {
		t.Fatalf("result %+v", result)
	}
}

func TestC3ProviderCredentialsDistinctAcrossRetainedAndRaw(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	id := strings.Repeat("a", 64)
	key := strings.Repeat("b", 64)
	request := strings.Repeat("c", 32)
	c3InsertRetained(t, s, start, key, "codex", id, request, true)
	event := loadFixtureEvents(t)[0]
	event.RequestedAt = start.Add(time.Hour + time.Minute)
	event.KeyID = key
	event.Provider = "codex"
	event.Succeeded = true
	event.ErrorClass = nil
	event.Outcome = ""
	event.ProxyRequestID = request
	event.CredentialID = &id
	algorithm := model.CredentialIDAlgorithm
	event.CredentialIDAlgorithm = &algorithm
	auth := "api-key"
	event.AuthType = &auth
	if err := s.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	values, err := s.ProviderCredentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].Requests != 1 || values[0].Failed != 1 || values[0].AuthType != auth || !values[0].ObservedAt.Equal(event.RequestedAt) {
		t.Fatalf("credentials %+v", values)
	}
}

func TestC3RetainedPricingMissingRangeErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	c3InsertRetained(t, s, start, strings.Repeat("a", 64), "codex", strings.Repeat("b", 64), strings.Repeat("c", 32), true)
	_, err := s.PricingMissing(ctx, model.Range{Start: start.Add(time.Minute), End: start.Add(time.Hour), TimeZone: "UTC"})
	if !errors.Is(err, ErrRetainedRangePartial) {
		t.Fatalf("partial range: %v", err)
	}
	_, err = s.PricingMissing(ctx, model.Range{Start: start, End: start.Add(time.Hour), TimeZone: "Asia/Tokyo"})
	if !errors.Is(err, ErrRetainedTimeZonePartial) {
		t.Fatalf("time zone: %v", err)
	}
}

func TestC3KeyCatalogRetainedMatchesSQLite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	key := strings.Repeat("a", 64)
	cred := strings.Repeat("b", 64)
	request := strings.Repeat("c", 32)
	event := c3V2Event(strings.Repeat("d", 32), request, key, start.Add(time.Minute), true, nil, 0, 0, 10, 20)
	event.Provider = "codex"
	event.Model = "test-model"
	event.Tokens.Input = 10
	event.Tokens.Output = 10
	event.Tokens.Total = 20
	event.CredentialID = &cred
	algorithm := model.CredentialIDAlgorithm
	event.CredentialIDAlgorithm = &algorithm
	auth := "oauth"
	event.AuthType = &auth
	sq, err := store.Open(ctx, store.Config{Path: filepath.Join(t.TempDir(), "a.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sq.Close(ctx) }()
	if err := sq.WriteBatch(ctx, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	if _, err := sq.ApplyRetention(ctx, start.Add(time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	ch := openTestStore(t)
	c3InsertRetained(t, ch, start, key, "codex", cred, request, false)
	if err := ch.db.Conn().Exec(ctx, "INSERT INTO "+ch.table("analytics_rollups")+" (grain,bucket_start_ns,bucket_end_ns,first_activity_ns,last_activity_ns,key_id,provider,model,credential_id,auth_type,succeeded,proxy_requests,upstream_attempts,total_tokens,unpriced_tokens,version,is_deleted) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", "hourly", start.UnixNano(), start.Add(time.Hour).UnixNano(), event.RequestedAt.UnixNano(), event.RequestedAt.UnixNano(), key, "codex", "test-model", cred, "oauth", int64(1), int64(1), int64(1), int64(20), int64(20), ch.nextVersion(), uint8(0)); err != nil {
		t.Fatal(err)
	}
	q := model.Query{SchemaVersion: 1, Operation: model.OperationDimensions, Dimension: "key", PageSize: 10, Start: start, End: start.Add(time.Hour), TimeZone: "UTC"}
	got, err := ch.KeyCatalog(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	want, err := sq.KeyCatalog(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Keys, want.Keys) {
		t.Fatalf("retained key catalog CH=%+v SQLite=%+v", got.Keys, want.Keys)
	}
}
