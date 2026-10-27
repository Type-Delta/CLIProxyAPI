package usage

import (
	"context"
	"strings"
	"testing"
)

func TestNormalizeProviderCredits(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		valid       bool
	}{
		{"0", "0", true}, {"0.000", "0", true}, {"1.2300e-2", "0.0123", true}, {"123e2", "12300", true}, {"1e-18", "0.000000000000000001", true}, {strings.Repeat("9", 64), strings.Repeat("9", 64), true},
		{"", "", false}, {"-0", "", false}, {"+1", "", false}, {"01", "", false}, {"1.", "", false}, {"NaN", "", false}, {"1e129", "", false}, {"1e-19", "", false}, {strings.Repeat("9", 65), "", false}, {strings.Repeat("0", 129), "", false},
	} {
		got, err := NormalizeProviderCredits(tc.input)
		if (err == nil) != tc.valid || got != tc.want {
			t.Errorf("%q => %q, %v; want %q valid=%t", tc.input, got, err, tc.want, tc.valid)
		}
	}
}

func TestProviderCreditsObserverBudget(t *testing.T) {
	manager := NewManager(8)
	records := make(chan Record, 2)
	if _, err := manager.RegisterNamed("credits", pluginFunc(func(_ context.Context, record Record) { records <- record })); err != nil {
		t.Fatal(err)
	}
	manager.Publish(context.Background(), Record{Provider: strings.Repeat("x", MaxObserverSnapshotBytes), Detail: Detail{ProviderCredits: "0.123456789012345678", RawUsage: strings.Repeat("x", MaxRawUsageBytes)}})
	manager.Publish(context.Background(), Record{Detail: Detail{ProviderCredits: strings.Repeat("9", 10000)}})
	closeManager(t, manager)
	first, second := <-records, <-records
	if first.Detail.ProviderCredits != "0.123456789012345678" {
		t.Fatalf("credits corrupted: %q", first.Detail.ProviderCredits)
	}
	if second.Detail.ProviderCredits != "" {
		t.Fatalf("invalid credits retained: %q", second.Detail.ProviderCredits)
	}
}
