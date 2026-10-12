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

func TestParseKiroGSRQuota(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantReset  time.Time
		wantPlan   string
		wantLimit  *int64
		wantUsed   *int64
		wantRemain *int64
		wantErr    bool
	}{
		{
			name:      "included reset with fractional seconds",
			body:      `{"subscriptionInfo":{"subscriptionTitle":"KIRO PRO"},"usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","currentUsage":1,"usageLimit":10,"nextDateReset":1893456000.5}]}`,
			wantReset: time.Unix(1893456000, 500000000).UTC(),
			wantPlan:  "KIRO PRO",
			wantLimit: usageProbeTestPtr(int64(10)), wantUsed: usageProbeTestPtr(int64(1)), wantRemain: usageProbeTestPtr(int64(9)),
		},
		{
			name:      "camel and precision fallbacks",
			body:      `{"SubscriptionInfo":{"subscription_title":"KIRO POWER"},"UsageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","currentUsageWithPrecision":"2","usageLimitWithPrecision":"20","nextDateResetUnix":"1893456000"}]}`,
			wantReset: time.Unix(1893456000, 0).UTC(),
			wantPlan:  "KIRO POWER",
			wantLimit: usageProbeTestPtr(int64(20)), wantUsed: usageProbeTestPtr(int64(2)), wantRemain: usageProbeTestPtr(int64(18)),
		},
		{
			name:    "trial-only response",
			body:    `{"subscriptionInfo":{"subscriptionTitle":"KIRO FREE"},"usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","freeTrialInfo":{"currentUsage":1,"usageLimit":10,"nextDateReset":1893456000}}]}`,
			wantErr: true,
		},
		{
			name:    "missing reset",
			body:    `{"subscriptionInfo":{"subscriptionTitle":"KIRO PRO"},"usageBreakdownList":[{"resourceType":"AGENTIC_REQUEST","currentUsage":1,"usageLimit":10}]}`,
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			windows, plan, errParse := parseKiroGSRQuota([]byte(test.body))
			if test.wantErr {
				if errParse == nil {
					t.Fatal("parseKiroGSRQuota() error = nil, want error")
				}
				return
			}
			if errParse != nil {
				t.Fatalf("parseKiroGSRQuota() error = %v", errParse)
			}
			if len(windows) != 1 || windows[0].Label != "monthly" || windows[0].ResetsAt == nil || !windows[0].ResetsAt.Equal(test.wantReset) {
				t.Fatalf("parsed windows = %#v, want monthly reset %v", windows, test.wantReset)
			}
			if plan != test.wantPlan {
				t.Fatalf("parsed plan = %q, want %q", plan, test.wantPlan)
			}
			for name, pair := range map[string]struct {
				got  *int64
				want *int64
			}{
				"limit":     {windows[0].Limit, test.wantLimit},
				"used":      {windows[0].Used, test.wantUsed},
				"remaining": {windows[0].Remaining, test.wantRemain},
			} {
				if pair.want == nil {
					if pair.got != nil {
						t.Fatalf("%s = %v, want nil", name, *pair.got)
					}
					continue
				}
				if pair.got == nil || *pair.got != *pair.want {
					t.Fatalf("%s = %v, want %v", name, pair.got, *pair.want)
				}
			}
		})
	}
}
