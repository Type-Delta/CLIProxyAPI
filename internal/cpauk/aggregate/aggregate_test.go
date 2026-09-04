package aggregate

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func TestPriceBookFixtureRules(t *testing.T) {
	input := model.NanoUSD(10_035_000_000)
	output := model.NanoUSD(15_052_500_000)
	aliasInput := model.NanoUSD(20_000_000_000)
	aliasOutput := model.NanoUSD(30_000_000_000)
	alias := "alias-53b1"
	book := PriceBook{Rules: []PricingRule{
		{ID: "model", Model: "model-f93b", InputPerMillion: &input, OutputPerMillion: &output, CacheReadMultiplier: "0", CacheCreationMultiplier: "0"},
		{ID: "alias", Alias: alias, InputPerMillion: &aliasInput, OutputPerMillion: &aliasOutput},
	}}
	event := model.Event{Model: "model-f93b", RequestedAlias: &alias, Tokens: model.TokenUsage{Input: 70, Output: 30, CacheRead: 5, Total: 100}}
	result, err := book.Price(event)
	if err != nil {
		t.Fatal(err)
	}
	if result.KnownCost == nil || result.KnownCost.String() != "0.00110385" || result.RuleID != "model" {
		t.Fatalf("unexpected price: %+v", result)
	}
}

func TestRangeDSTAndMonday(t *testing.T) {
	now := time.Date(2026, 3, 8, 16, 0, 0, 0, time.UTC)
	start, end, err := ResolveRange(RangeToday, now, "America/New_York", 0)
	if err != nil {
		t.Fatal(err)
	}
	if end.Sub(start) != 23*time.Hour {
		t.Fatalf("spring day = %s", end.Sub(start))
	}
	now = time.Date(2026, 3, 12, 16, 0, 0, 0, time.UTC)
	start, _, err = ResolveRange(RangeCalendarWeek, now, "America/New_York", 0)
	if err != nil {
		t.Fatal(err)
	}
	if start.Format(time.RFC3339) != "2026-03-09T04:00:00Z" {
		t.Fatalf("week start = %s", start)
	}
}

func TestAddedCalendarRangesAcrossZones(t *testing.T) {
	now := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)
	for _, zone := range []string{"America/St_Johns", "Asia/Kolkata"} {
		t.Run(zone, func(t *testing.T) {
			location, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			localNow := now.In(location)
			day := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location)
			daysSinceMonday := (int(localNow.Weekday()) + 6) % 7
			currentWeekStart := day.AddDate(0, 0, -daysSinceMonday)
			currentMonthStart := time.Date(localNow.Year(), localNow.Month(), 1, 0, 0, 0, 0, location)
			currentYearStart := time.Date(localNow.Year(), time.January, 1, 0, 0, 0, 0, location)
			cases := []struct {
				name      string
				kind      RangeKind
				wantStart time.Time
				wantEnd   time.Time
			}{
				{"previous week", RangePreviousWeek, currentWeekStart.AddDate(0, 0, -7), currentWeekStart},
				{"previous month", RangePreviousMonth, currentMonthStart.AddDate(0, -1, 0), currentMonthStart},
				{"current year", RangeCalendarYear, currentYearStart, currentYearStart.AddDate(1, 0, 0)},
				{"previous year", RangePreviousYear, currentYearStart.AddDate(-1, 0, 0), currentYearStart},
			}
			for _, testCase := range cases {
				t.Run(testCase.name, func(t *testing.T) {
					start, end, err := ResolveRange(testCase.kind, now, zone, 0)
					if err != nil {
						t.Fatal(err)
					}
					if !start.Equal(testCase.wantStart.UTC()) || !end.Equal(testCase.wantEnd.UTC()) {
						t.Fatalf("range=%s..%s, want %s..%s", start, end, testCase.wantStart.UTC(), testCase.wantEnd.UTC())
					}
				})
			}
		})
	}
}

func TestLatencyFixtures(t *testing.T) {
	bins, err := LatencyBins([]int64{10, 20, 30, 40, 50})
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{116, 150, 171, 185, 196}
	for index := range want {
		if bins[index].Index != want[index] {
			t.Fatalf("bin %d = %d, want %d", index, bins[index].Index, want[index])
		}
	}
	encoded, err := MarshalLatencyBins(bins)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(encoded) != "0105e80101ac0201d60201f20201880301" {
		t.Fatalf("encoded sketch = %x", encoded)
	}
	p90 := Percentile([]int64{10, 20, 30, 40, 50}, .9)
	if p90 == nil || *p90 != 50 {
		t.Fatalf("p90 = %v", p90)
	}
}
