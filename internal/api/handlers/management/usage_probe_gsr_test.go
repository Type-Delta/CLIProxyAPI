package management

import (
	"testing"
	"time"
)

func TestParseCodexGSRQuotaUsesWeeklyWindow(t *testing.T) {
	windows, errParse := parseCodexGSRQuota([]byte(`{"rate_limit":{"primary_window":{"window_minutes":10080,"reset_after_seconds":3600},"secondary_window":{"window_minutes":60,"reset_after_seconds":30}}}`))
	if errParse != nil {
		t.Fatalf("parseCodexGSRQuota() error = %v", errParse)
	}
	if len(windows) != 1 || windows[0].ResetsAt == nil {
		t.Fatalf("parsed windows = %#v, want one reset window", windows)
	}
	remaining := time.Until(*windows[0].ResetsAt)
	if remaining < 59*time.Minute || remaining > 61*time.Minute {
		t.Fatalf("weekly reset remaining = %v, want about 1h", remaining)
	}
}

func TestParseCodexGSRQuotaUsesCamelCaseResetAfter(t *testing.T) {
	windows, errParse := parseCodexGSRQuota([]byte(`{"rateLimit":{"primaryWindow":{"windowMinutes":10080,"resetAfterSeconds":120}}}`))
	if errParse != nil || len(windows) != 1 || windows[0].ResetsAt == nil {
		t.Fatalf("camelCase Codex parse = %#v, %v", windows, errParse)
	}
	if remaining := time.Until(*windows[0].ResetsAt); remaining < 119*time.Second || remaining > 121*time.Second {
		t.Fatalf("camelCase reset remaining = %v, want about 2m", remaining)
	}
}

func TestParseClaudeGSRQuotaSeparatesFableWindow(t *testing.T) {
	windows, errParse := parseClaudeGSRQuota([]byte(`{"seven_day":{"resets_at":"2030-01-02T00:00:00Z"},"limits":[{"kind":"weekly_scoped","percent":10,"resets_at":"2030-01-03T00:00:00Z","is_active":true,"scope":{"model":{"display_name":"fable"}}}]}`))
	if errParse != nil {
		t.Fatalf("parseClaudeGSRQuota() error = %v", errParse)
	}
	if len(windows) != 2 || windows[0].Label != "weekly" || windows[1].Label != "weekly_fable" {
		t.Fatalf("parsed windows = %#v, want weekly and weekly_fable", windows)
	}
}
