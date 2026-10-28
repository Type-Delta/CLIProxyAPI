package chstore

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

var c2r2LeaderboardStart = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
var c2r2LeaderboardEnd = c2r2LeaderboardStart.Add(7 * 24 * time.Hour)

func c2r2NewLeaderboardStore(t *testing.T) *Store {
	t.Helper()
	database := openTestStore(t, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	return database
}

func c2r2LeaderboardQuery() model.Query {
	return c2r2V2Query(model.OperationModelLeaderboard, c2r2LeaderboardStart, c2r2LeaderboardEnd)
}

func c2r2LeaderboardEvent(number int, modelName, providerName string, succeeded bool) model.Event {
	requestedAt := c2r2LeaderboardStart.Add(time.Duration(number) * time.Minute)
	event := c2r2V2Event(fmt.Sprintf("%032x", number), fmt.Sprintf("%032x", number+1_000_000),
		strings.Repeat("a", 64), requestedAt, succeeded, nil, 0, 0, 1, 2)
	event.Model, event.Provider = modelName, providerName
	return event
}

func c2r2Session(value string) *string {
	result := strings.Repeat(value, 64)
	return &result
}

func c2r2WriteLeaderboardEvents(t *testing.T, database *Store, events []model.Event) {
	t.Helper()
	if err := database.WriteBatch(context.Background(), events); err != nil {
		t.Fatal(err)
	}
}

func c2r2AwardRows(t *testing.T, result model.ModelLeaderboard, metric string) []model.ModelLeaderboardRow {
	t.Helper()
	for _, award := range result.Awards {
		if award.Metric == metric {
			return award.Rows
		}
	}
	t.Fatalf("award %q is missing", metric)
	return nil
}

func TestModelLeaderboardEmptyStoreAlwaysHasFourEmptyAwards(t *testing.T) {
	t.Parallel()
	database := c2r2NewLeaderboardStore(t)
	result, err := database.ModelLeaderboard(context.Background(), c2r2LeaderboardQuery())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Awards) != 4 {
		t.Fatalf("awards = %d", len(result.Awards))
	}
	wantMetrics := []string{"most_used", "most_reliable", "speed", "most_talkative"}
	for index, award := range result.Awards {
		if award.Metric != wantMetrics[index] || award.Rows == nil || len(award.Rows) != 0 {
			t.Fatalf("award %d = %+v", index, award)
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"start":"2026-10-01T00:00:00Z","end":"2026-10-08T00:00:00Z","awards":[{"metric":"most_used","rows":[]},{"metric":"most_reliable","rows":[]},{"metric":"speed","rows":[]},{"metric":"most_talkative","rows":[]}]}`
	if string(encoded) != want {
		t.Fatalf("empty leaderboard JSON = %s", encoded)
	}
}

func TestModelLeaderboardMostUsedCountsSessionsAcrossProvidersAndBreaksTies(t *testing.T) {
	t.Parallel()
	database := c2r2NewLeaderboardStore(t)
	events := []model.Event{
		c2r2LeaderboardEvent(1, "alpha", "provider-a", true),
		c2r2LeaderboardEvent(2, "alpha", "provider-b", true),
		c2r2LeaderboardEvent(3, "alpha", "provider-a", true),
		c2r2LeaderboardEvent(4, "beta", "provider-a", true),
		c2r2LeaderboardEvent(5, "zeta", "provider-a", true),
	}
	events[0].SessionDigest, events[1].SessionDigest, events[2].SessionDigest = c2r2Session("1"), c2r2Session("1"), c2r2Session("2")
	events[3].SessionDigest, events[4].SessionDigest = c2r2Session("3"), c2r2Session("4")
	c2r2WriteLeaderboardEvents(t, database, events)
	result, err := database.ModelLeaderboard(context.Background(), c2r2LeaderboardQuery())
	if err != nil {
		t.Fatal(err)
	}
	rows := c2r2AwardRows(t, result, "most_used")
	if len(rows) != 3 || rows[0].Model != "alpha" || rows[0].Value != 2 || rows[0].Providers == nil || *rows[0].Providers != 2 {
		t.Fatalf("most_used = %+v", rows)
	}
	if rows[1].Model != "beta" || rows[2].Model != "zeta" || rows[1].Rank != 2 || rows[2].Rank != 3 {
		t.Fatalf("most_used tie order = %+v", rows)
	}
}

func TestModelLeaderboardSpeedFloorAndPublishedTPSOnly(t *testing.T) {
	t.Parallel()
	database := c2r2NewLeaderboardStore(t)
	makeSpeed := func(number int, name string, output, generation int64, tps bool) model.Event {
		event := c2r2LeaderboardEvent(number, name, "provider-a", true)
		event.Tokens.Output = output
		if tps {
			event.GenerationTimeMS = &generation
			event.OutputTiming = &model.OutputTiming{Version: 1, Protocol: "chat", Source: "source_reader", Status: "observed", FirstArrivalNS: 1, LastArrivalNS: 1 + generation*int64(time.Millisecond), ArrivalCount: 5, WindowTokens: output, TokenQuality: model.TokenQualityEstimated, Coverage: "visible_deltas"}
		}
		return event
	}
	events := []model.Event{
		makeSpeed(1, "slow-79.9", 799, 10_000, true),
		makeSpeed(2, "keep-80", 800, 10_000, true),
		makeSpeed(3, "tie-a", 1_000, 1_000, true),
		makeSpeed(4, "tie-b", 1_000, 1_000, true),
		makeSpeed(5, "tie-b", 1_000, 1_000, true),
		makeSpeed(6, "no-tps", 10_000, 0, false),
	}
	failed := makeSpeed(7, "failed-fast", 5000, 1000, true)
	failed.Succeeded = false
	events = append(events, failed)
	c2r2WriteLeaderboardEvents(t, database, events)
	result, err := database.ModelLeaderboard(context.Background(), c2r2LeaderboardQuery())
	if err != nil {
		t.Fatal(err)
	}
	rows := c2r2AwardRows(t, result, "speed")
	if len(rows) != 3 {
		t.Fatalf("speed rows = %+v", rows)
	}
	if rows[0].Model != "tie-b" || rows[0].Samples == nil || *rows[0].Samples != 2 || rows[0].Value != 1_000 {
		t.Fatalf("speed sample tie-break = %+v", rows[0])
	}
	if rows[1].Model != "tie-a" || rows[2].Model != "keep-80" || rows[2].Value != 80 {
		t.Fatalf("speed floor order = %+v", rows)
	}
	for _, row := range rows {
		if row.Model == "slow-79.9" || row.Model == "no-tps" {
			t.Fatalf("excluded speed model ranked: %+v", row)
		}
	}
}

func TestModelLeaderboardReliabilityFloorPodium(t *testing.T) {
	t.Parallel()
	database := c2r2NewLeaderboardStore(t)
	addModel := func(number int, name string, successes, failures int) []model.Event {
		events := make([]model.Event, 0, successes+failures)
		for index := 0; index < successes+failures; index++ {
			events = append(events, c2r2LeaderboardEvent(number+index, name, "provider-a", index < successes))
		}
		return events
	}
	c2r2WriteLeaderboardEvents(t, database, addModel(1, "at-floor", 200, 0))
	result, err := database.ModelLeaderboard(context.Background(), c2r2LeaderboardQuery())
	if err != nil {
		t.Fatal(err)
	}
	rows := c2r2AwardRows(t, result, "most_reliable")
	if len(rows) != 1 || rows[0].Model != "at-floor" || rows[0].Attempts == nil || *rows[0].Attempts != 200 {
		t.Fatalf("one-row reliability podium = %+v", rows)
	}

	c2r2WriteLeaderboardEvents(t, database, addModel(1_000, "half-failed", 100, 100))
	result, err = database.ModelLeaderboard(context.Background(), c2r2LeaderboardQuery())
	if err != nil {
		t.Fatal(err)
	}
	rows = c2r2AwardRows(t, result, "most_reliable")
	if len(rows) != 2 || rows[0].Model != "at-floor" || rows[1].Model != "half-failed" {
		t.Fatalf("two-row reliability podium = %+v", rows)
	}
	if rows[1].Failed == nil || *rows[1].Failed != 100 || rows[1].Value != 0.5 {
		t.Fatalf("reliability failure row = %+v", rows[1])
	}
}

func TestModelLeaderboardReliabilityRejects199Attempts(t *testing.T) {
	t.Parallel()
	database := c2r2NewLeaderboardStore(t)
	events := make([]model.Event, 199)
	for index := range events {
		events[index] = c2r2LeaderboardEvent(index+1, "below-floor", "provider-a", true)
	}
	c2r2WriteLeaderboardEvents(t, database, events)
	result, err := database.ModelLeaderboard(context.Background(), c2r2LeaderboardQuery())
	if err != nil {
		t.Fatal(err)
	}
	if rows := c2r2AwardRows(t, result, "most_reliable"); len(rows) != 0 {
		t.Fatalf("199 attempts qualified: %+v", rows)
	}
}

func TestModelLeaderboardTalkativeDoesNotDoubleCountNormalizedReasoning(t *testing.T) {
	t.Parallel()
	database := c2r2NewLeaderboardStore(t)
	events := []model.Event{}
	for index, values := range [][2]int64{{100, 50}, {200, 50}, {300, 50}} {
		event := c2r2LeaderboardEvent(index+1, "inclusive", "provider-a", true)
		event.Tokens.Output, event.Tokens.Reasoning = values[0], values[1]
		events = append(events, event)
	}
	zero := c2r2LeaderboardEvent(10, "zero", "provider-a", true)
	zero.Tokens.Output, zero.Tokens.Reasoning = 0, 0
	failed := c2r2LeaderboardEvent(11, "failed", "provider-a", false)
	failed.Tokens.Output, failed.Tokens.Reasoning = 10_000, 0
	events = append(events, zero, failed)
	c2r2WriteLeaderboardEvents(t, database, events)
	result, err := database.ModelLeaderboard(context.Background(), c2r2LeaderboardQuery())
	if err != nil {
		t.Fatal(err)
	}
	rows := c2r2AwardRows(t, result, "most_talkative")
	if len(rows) != 1 || rows[0].Model != "inclusive" || rows[0].Value != 300 || rows[0].Samples == nil || *rows[0].Samples != 3 {
		t.Fatalf("talkative rows = %+v", rows)
	}
	if rows[0].ReasoningShare == nil || *rows[0].ReasoningShare != 0.25 {
		t.Fatalf("reasoning share = %v", rows[0].ReasoningShare)
	}
}

func TestModelLeaderboardExcludesRawOutputFallbackFromTalkative(t *testing.T) {
	t.Parallel()
	database := c2r2NewLeaderboardStore(t)
	events := make([]model.Event, 200)
	generationMS := int64(1_000)
	for index := range events {
		event := c2r2LeaderboardEvent(index+1, "raw-fallback", "provider-a", true)
		event.Tokens = model.TokenUsage{
			Output: 100, Reasoning: 40, Total: 140,
			Schema: model.AccountingSchemaRawFallbackV1, Quality: model.TokenQualityExact,
		}
		event.GenerationTimeMS = &generationMS
		digest := fmt.Sprintf("%064x", index+1)
		event.SessionDigest = &digest
		events[index] = event
	}
	c2r2WriteLeaderboardEvents(t, database, events)
	result, err := database.ModelLeaderboard(context.Background(), c2r2LeaderboardQuery())
	if err != nil {
		t.Fatal(err)
	}
	if rows := c2r2AwardRows(t, result, "most_talkative"); len(rows) != 0 {
		t.Fatalf("raw fallback entered talkative award: %+v", rows)
	}
	if rows := c2r2AwardRows(t, result, "most_used"); len(rows) != 1 || rows[0].Value != 200 {
		t.Fatalf("raw fallback most_used = %+v", rows)
	}
	if rows := c2r2AwardRows(t, result, "most_reliable"); len(rows) != 1 || rows[0].Attempts == nil || *rows[0].Attempts != 200 {
		t.Fatalf("raw fallback reliability = %+v", rows)
	}
	if rows := c2r2AwardRows(t, result, "speed"); len(rows) != 0 {
		t.Fatalf("raw fallback speed = %+v", rows)
	}
}

func TestModelLeaderboardCapsAwardsAtSixRows(t *testing.T) {
	t.Parallel()
	database := c2r2NewLeaderboardStore(t)
	var events []model.Event
	attempt := 1
	for modelIndex := 1; modelIndex <= 7; modelIndex++ {
		for sessionIndex := 0; sessionIndex < 8-modelIndex; sessionIndex++ {
			event := c2r2LeaderboardEvent(attempt, fmt.Sprintf("model-%d", modelIndex), "provider-a", true)
			digest := fmt.Sprintf("%064x", modelIndex*16+sessionIndex)
			event.SessionDigest = &digest
			events = append(events, event)
			attempt++
		}
	}
	c2r2WriteLeaderboardEvents(t, database, events)
	result, err := database.ModelLeaderboard(context.Background(), c2r2LeaderboardQuery())
	if err != nil {
		t.Fatal(err)
	}
	rows := c2r2AwardRows(t, result, "most_used")
	if len(rows) != 6 {
		t.Fatalf("most_used rows = %d", len(rows))
	}
	for index, row := range rows {
		if row.Rank != index+1 {
			t.Fatalf("rank at %d = %d", index, row.Rank)
		}
	}
	if rows[0].Model != "model-1" || rows[5].Model != "model-6" {
		t.Fatalf("capped order = %+v", rows)
	}
}
