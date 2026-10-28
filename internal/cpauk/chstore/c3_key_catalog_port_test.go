package chstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestKeyCatalogLifecyclePaginationAndRetainedActivity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openTestStore(t)
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
	want := c3CollectKeyCatalog(t, database, query)
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
	c3AssertKeyCatalogEqual(t, c3CollectKeyCatalog(t, database, query), want)
	if _, err := database.ApplyRetentionPolicy(ctx, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 1); err != nil {
		t.Fatal(err)
	}
	c3AssertKeyCatalogEqual(t, c3CollectKeyCatalog(t, database, query), want)
}

func c3CollectKeyCatalog(t *testing.T, database *Store, query model.Query) map[string]model.KeyIdentity {
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

func c3AssertKeyCatalogEqual(t *testing.T, got, want map[string]model.KeyIdentity) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("key catalog length=%d want=%d", len(got), len(want))
	}
	for keyID, expected := range want {
		actual, exists := got[keyID]
		if !exists || actual.KeyID != expected.KeyID || actual.ShortKeyID != expected.ShortKeyID ||
			actual.Status != expected.Status || actual.TotalTokens != expected.TotalTokens ||
			actual.KnownCost != expected.KnownCost || actual.UnpricedTokens != expected.UnpricedTokens ||
			!c3EqualOptionalTime(actual.FirstActivityAt, expected.FirstActivityAt) ||
			!c3EqualOptionalTime(actual.LastActivityAt, expected.LastActivityAt) {
			t.Fatalf("key catalog %s\n got: %+v\nwant: %+v", keyID, actual, expected)
		}
	}
}

func c3EqualOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func TestKeyCatalogReportsTopCostModelSeparately(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openTestStore(t)
	priced := loadFixtureEvents(t)[0]
	// The unpriced model leads on tokens while the priced fixture model carries all the cost.
	unpriced := priced
	unpriced.AttemptID = strings.Repeat("e", 32)
	unpriced.ProxyRequestID = strings.Repeat("f", 32)
	unpriced.Model = "model-unpriced"
	unpriced.Tokens.Input, unpriced.Tokens.Total = 5_000, 5_030
	if err := database.WriteBatch(ctx, []model.Event{priced, unpriced}); err != nil {
		t.Fatal(err)
	}
	query := model.Query{SchemaVersion: 1, Operation: model.OperationDimensions, Dimension: "key", PageSize: 10,
		Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), TimeZone: "UTC"}
	key := c3CollectKeyCatalog(t, database, query)[priced.KeyID]
	if key.TopModel == nil || *key.TopModel != "model-unpriced" || key.TopModelTokens != 5_030 {
		t.Fatalf("top token model = %v/%d", key.TopModel, key.TopModelTokens)
	}
	if key.TopCostModel == nil || *key.TopCostModel != "model-f93b" || key.TopCostModelUSD <= 0 || key.TopCostModelUSD != key.KnownCost {
		t.Fatalf("top cost model = %v/%s, key cost %s", key.TopCostModel, key.TopCostModelUSD, key.KnownCost)
	}
}
