package chstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/aggregate"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func openTestStore(t testing.TB, configs ...Config) *Store {
	t.Helper()
	config := Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20, PriceBook: fixturePriceBook()}
	if len(configs) > 0 {
		config = configs[0]
	}
	handle := chtest.Handle(t)
	s, err := Open(context.Background(), handle, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(config.Path); os.IsNotExist(err) {
		if err := RunLegacyImport(context.Background(), s, config.Path); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Errorf("close analytics: %v", err)
		}
	})
	return s
}
func loadFixtureEvents(t testing.TB) []model.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "upstream-v1.15.0", "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Events []model.Event `json:"events"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture.Events
}
func fixturePriceBook() aggregate.PriceBook {
	input := model.NanoUSD(10_035_000_000)
	output := model.NanoUSD(15_052_500_000)
	return aggregate.PriceBook{Rules: []aggregate.PricingRule{{ID: "price-04e81c", Model: "model-f93b", InputPerMillion: &input, OutputPerMillion: &output, CacheReadMultiplier: "0", CacheCreationMultiplier: "0", Source: "fixture-9a70e2"}}}
}
func insertFixtureEvents(t testing.TB, s *Store) []model.Event {
	t.Helper()
	events := loadFixtureEvents(t)
	if err := s.WriteBatch(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	return events
}
