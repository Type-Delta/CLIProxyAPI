package management

import (
	"reflect"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Bar IDs here must match the IDs CPAMC's quota adapters render, or a reserve set
// in the UI would never match a bar evaluated by routing. Fixtures follow the
// adapters' own tests under web/management-center/tests.

func barSummary(bars []coreauth.QuotaReserveBar) map[string]float64 {
	out := make(map[string]float64, len(bars))
	for _, bar := range bars {
		out[bar.ID] = bar.RemainingPercent
	}
	return out
}

func TestCodexQuotaReserveBarsMatchCPAMCIDs(t *testing.T) {
	body := []byte(`{
		"rate_limit": {"allowed": true, "primary_window": {"used_percent": 1, "limit_window_seconds": 604800, "reset_at": 1785902974}, "secondary_window": null},
		"code_review_rate_limit": null,
		"additional_rate_limits": [{"limit_name": "GPT-5.3-Codex-Spark", "rate_limit": {"primary_window": {"used_percent": 0, "limit_window_seconds": 604800, "reset_at": 1785903197}}}]
	}`)
	bars, err := codexQuotaReserveBars(body)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"weekly": 99, "gpt-5-3-codex-spark-weekly-0": 100}
	if got := barSummary(bars); !reflect.DeepEqual(got, want) {
		t.Fatalf("bars = %v, want %v", got, want)
	}

	twoWindows := []byte(`{"rate_limit": {
		"primary_window": {"used_percent": 40, "limit_window_seconds": 18000, "reset_after_seconds": 60},
		"secondary_window": {"used_percent": 75, "limit_window_seconds": 2592000, "reset_at": 1785902974}}}`)
	bars, err = codexQuotaReserveBars(twoWindows)
	if err != nil {
		t.Fatal(err)
	}
	want = map[string]float64{"five-hour": 60, "monthly": 25}
	if got := barSummary(bars); !reflect.DeepEqual(got, want) {
		t.Fatalf("bars = %v, want %v", got, want)
	}
	if bars[0].ResetAt.IsZero() {
		t.Fatal("reset_after_seconds was not converted to a reset time")
	}
}

func TestClaudeQuotaReserveBarsMatchCPAMCIDs(t *testing.T) {
	body := []byte(`{
		"five_hour": {"utilization": 12.5, "resets_at": "2026-10-14T12:00:00Z"},
		"seven_day": {"utilization": 70, "resets_at": "2026-10-20T00:00:00Z"},
		"seven_day_opus": null,
		"iguana_necktie": {"utilization": 99, "resets_at": "2026-10-20T00:00:00Z"},
		"limits": [{"kind": "weekly_scoped", "percent": 40, "resets_at": "2026-10-21T00:00:00Z", "is_active": true, "scope": {"model": {"display_name": "Fable"}}}]
	}`)
	bars, err := claudeQuotaReserveBars(body)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"five-hour": 87.5, "seven-day": 30, "seven-day-fable": 60}
	if got := barSummary(bars); !reflect.DeepEqual(got, want) {
		t.Fatalf("bars = %v, want %v", got, want)
	}
}

func TestZaiAndOpenCodeGoQuotaReserveBarsMatchCPAMCIDs(t *testing.T) {
	zai := []byte(`{"code": 200, "success": true, "data": {"limits": [
		{"type": "TOKENS_LIMIT", "unit": 3, "number": 5, "percentage": 25, "nextResetTime": 1785902974000},
		{"type": "TOKENS_LIMIT", "unit": 6, "number": 1, "percentage": 80}]}}`)
	bars, err := zaiQuotaReserveBars(zai)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := barSummary(bars), map[string]float64{"zai-hour-5": 75, "zai-week-1": 20}; !reflect.DeepEqual(got, want) {
		t.Fatalf("zai bars = %v, want %v", got, want)
	}

	opencode := []byte(`{"usage": {"rolling": {"percent": 10, "resetsAt": "2026-10-14T12:00:00Z"}, "weekly": {"percent": 5, "status": "rate_limited"}}}`)
	bars, err = opencodeGoQuotaReserveBars(opencode)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := barSummary(bars), map[string]float64{"opencode-go-rolling": 90, "opencode-go-weekly": 0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("opencode bars = %v, want %v", got, want)
	}
}

func TestKiroQuotaReserveBarsMatchCPAMCIDs(t *testing.T) {
	body := []byte(`{"usageBreakdownList": [{
		"resourceType": "CREDIT", "currentUsageWithPrecision": 250, "usageLimitWithPrecision": 1000, "nextDateReset": 1785902974,
		"freeTrialInfo": {"currentUsage": 50, "usageLimit": 50}}]}`)
	bars, err := kiroQuotaReserveBars(body)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"kiro-included-credit-0": 75, "kiro-trial-agentic_request-0": 0}
	if got := barSummary(bars); !reflect.DeepEqual(got, want) {
		t.Fatalf("bars = %v, want %v", got, want)
	}
}
