package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func TestKeyCatalogLifecyclePaginationAndRetainedActivity(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20,
		PriceBook: fixturePriceBook()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	events := loadFixtureEvents(t)
	zeroActivityKeyID := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateKeyLifecycle(ctx, []string{events[0].KeyID, events[1].KeyID}, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateKeyLifecycle(ctx, []string{events[2].KeyID, zeroActivityKeyID}, []string{events[0].KeyID}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateKeyLifecycle(ctx, []string{"raw-key"}, nil); err == nil {
		t.Fatal("raw key was accepted by lifecycle persistence")
	}
	query := model.Query{SchemaVersion: 1, Operation: model.OperationDimensions, Dimension: "key", PageSize: 1,
		Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), TimeZone: "UTC"}
	want := collectKeyCatalog(t, database, query)
	if len(want) != 4 || want[events[0].KeyID].Status != model.KeyStatusRotated ||
		want[events[1].KeyID].Status != model.KeyStatusDeleted || want[events[2].KeyID].Status != model.KeyStatusConfigured {
		t.Fatalf("key lifecycle catalog = %+v", want)
	}
	if zero := want[zeroActivityKeyID]; zero.Status != model.KeyStatusConfigured || zero.FirstActivityAt != nil || zero.LastActivityAt != nil {
		t.Fatalf("zero-activity configured key = %+v", zero)
	}
	for _, event := range events {
		item := want[event.KeyID]
		if item.FirstActivityAt == nil || item.LastActivityAt == nil || !item.FirstActivityAt.Equal(event.RequestedAt) || !item.LastActivityAt.Equal(event.RequestedAt) {
			t.Fatalf("key activity for %s = %+v", event.KeyID, item)
		}
	}
	if _, err := database.ApplyRetentionPolicy(ctx, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Time{}, 1); err != nil {
		t.Fatal(err)
	}
	assertKeyCatalogEqual(t, collectKeyCatalog(t, database, query), want)
	if _, err := database.ApplyRetentionPolicy(ctx, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 1); err != nil {
		t.Fatal(err)
	}
	assertKeyCatalogEqual(t, collectKeyCatalog(t, database, query), want)
}

func collectKeyCatalog(t *testing.T, database *SQLiteStore, query model.Query) map[string]model.KeyIdentity {
	t.Helper()
	result := map[string]model.KeyIdentity{}
	for {
		page, err := database.KeyCatalog(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Keys {
			if _, exists := result[item.KeyID]; exists {
				t.Fatalf("duplicate key catalog row %s", item.KeyID)
			}
			result[item.KeyID] = item
		}
		if page.Meta.NextCursor == "" {
			return result
		}
		query.Cursor = page.Meta.NextCursor
	}
}

func assertKeyCatalogEqual(t *testing.T, got, want map[string]model.KeyIdentity) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("key catalog length=%d want=%d", len(got), len(want))
	}
	for keyID, expected := range want {
		actual, exists := got[keyID]
		if !exists || actual.KeyID != expected.KeyID || actual.ShortKeyID != expected.ShortKeyID ||
			actual.Status != expected.Status || actual.TotalTokens != expected.TotalTokens ||
			actual.KnownCost != expected.KnownCost || actual.UnpricedTokens != expected.UnpricedTokens ||
			!equalOptionalTime(actual.FirstActivityAt, expected.FirstActivityAt) ||
			!equalOptionalTime(actual.LastActivityAt, expected.LastActivityAt) {
			t.Fatalf("key catalog %s\n got: %+v\nwant: %+v", keyID, actual, expected)
		}
	}
}

func equalOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}
