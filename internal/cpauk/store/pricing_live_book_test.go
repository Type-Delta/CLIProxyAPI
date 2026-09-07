package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func TestPricingDiscoveryUpdatesSubsequentIngestPrices(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	fetcher := &testPricingFetcher{catalog: testPricingCatalog(1_000_000_000, 2_000_000_000)}
	database := openDiscoveryStore(t, filepath.Join(t.TempDir(), "analytics.db"), fetcher, &now)
	for index, want := range []int64{300_000, 600_000} {
		if index > 0 {
			now = now.Add(PricingCatalogTTL)
			fetcher.mu.Lock()
			fetcher.catalog = testPricingCatalog(2_000_000_000, 4_000_000_000)
			fetcher.mu.Unlock()
		}
		if _, err := database.RefreshPricing(ctx); err != nil {
			t.Fatal(err)
		}
		event := v2Event(fmt.Sprintf("%032x", index+1), fmt.Sprintf("%032x", index+10), strings.Repeat("a", 64), now, true, nil, 0, 0, 10, 20)
		event.Provider, event.Model = "codex", "gpt-5"
		if err := database.WriteBatch(ctx, []model.Event{event}); err != nil {
			t.Fatal(err)
		}
		var cost sql.NullInt64
		var unpriced int64
		if err := database.db.QueryRowContext(ctx, "SELECT known_cost_nano, unpriced_tokens FROM events WHERE attempt_id=?", event.AttemptID).Scan(&cost, &unpriced); err != nil {
			t.Fatal(err)
		}
		if !cost.Valid || cost.Int64 != want || unpriced != 0 {
			t.Errorf("catalog version %d stored cost=%+v unpriced=%d, want cost=%d unpriced=0", index+1, cost, unpriced, want)
		}
	}
}
