package management

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// RunQuotaHistoryCollector samples supported credentials hourly, spacing upstream reads.
// Cancellation interrupts both the spacing wait and any in-flight request.
func (h *Handler) RunQuotaHistoryCollector(ctx context.Context) {
	if h == nil || h.authManager == nil {
		return
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		h.collectQuotaHistory(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *Handler) collectQuotaHistory(ctx context.Context) {
	service, err := h.quotaHistoryService()
	if err != nil {
		return
	}
	capabilities := h.analyticsService().Capabilities()
	if !capabilities.Enabled || !capabilities.Available {
		return
	}
	for _, auth := range h.authManager.List() {
		if ctx.Err() != nil {
			return
		}
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		provider := coreauth.QuotaRecoveryProviderForAuth(auth)
		if provider == "" {
			continue
		}
		samples, errProbe := h.collectQuotaHistoryCredential(ctx, auth, provider, service)
		if errProbe == nil && len(samples) > 0 {
			if errWrite := service.RecordQuotaHistory(ctx, samples); errWrite != nil {
				log.WithError(errWrite).Debug("quota history write failed")
			}
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (h *Handler) collectQuotaHistoryCredential(ctx context.Context, auth *coreauth.Auth, provider string, service quotaHistoryService) ([]store.QuotaHistorySample, error) {
	auth.EnsureIndex()
	id, err := service.CredentialID(provider, auth.Index, auth.ID)
	if err != nil || id == nil {
		return nil, err
	}
	plan := strings.TrimSpace(authAttribute(auth, "plan_type"))
	if plan == "" {
		plan, _ = auth.Metadata["plan_type"].(string)
	}
	var samples []store.QuotaHistorySample
	appendWindow := func(window string, remaining float64, reset *time.Time, hours, capacity *float64) {
		if math.IsNaN(remaining) || math.IsInf(remaining, 0) || remaining < 0 || remaining > 100 {
			return
		}
		if plan != "" && !quotaHistoryIdentifier.MatchString(plan) {
			plan = ""
		}
		samples = append(samples, store.QuotaHistorySample{Provider: provider, CredentialID: *id, WindowID: window, RemainingPercent: remaining, ResetAt: reset, PeriodHours: hours, Capacity: capacity, PlanID: plan, Source: "cpa_auto"})
	}
	var endpoint, name string
	switch provider {
	case "claude":
		endpoint, name = claudeUsageQuotaURL, "Claude"
	case "codex":
		endpoint, name = codexUsageQuotaURL, "Codex"
	case "opencode-go":
		endpoint, name = opencodeGoUsageQuotaURL, "OpenCode Go"
	case "zai":
		endpoint, name = zaiUsageQuotaURL, "Z.ai"
	case "kiro":
		endpoint, name = kiroUsageLimitsURL(auth), "Kiro"
	default:
		return nil, fmt.Errorf("unsupported quota provider")
	}
	_, err = runUsageProbeWindowsWithMode(ctx, h, auth, name, endpoint, func(body []byte) ([]model.ProviderQuotaWindow, error) {
		root := gjson.ParseBytes(body)
		if !gjson.ValidBytes(body) {
			return nil, fmt.Errorf("invalid quota response")
		}
		switch provider {
		case "claude":
			period := 168.0
			add := func(id string, row gjson.Result) {
				used := firstGJSON(row, "utilization", "percent")
				if !used.Exists() {
					return
				}
				var reset *time.Time
				if parsed, e := time.Parse(time.RFC3339, row.Get("resets_at").String()); e == nil {
					value := parsed.UTC()
					reset = &value
				}
				appendWindow(id, 100-used.Float(), reset, &period, nil)
			}
			add("seven-day", root.Get("seven_day"))
			hasScoped := false
			for _, row := range root.Get("limits").Array() {
				if row.Get("kind").String() == "weekly_scoped" && strings.EqualFold(row.Get("scope.model.display_name").String(), "fable") {
					add("seven-day-fable", row)
					hasScoped = true
					break
				}
			}
			if !hasScoped {
				add("seven-day-fable", root.Get("iguana_necktie"))
			}
		case "codex":
			if raw := firstGJSON(root, "plan_type", "planType").String(); raw != "" {
				plan = raw
			}
			rate := firstGJSON(root, "rate_limit", "rateLimit")
			var best gjson.Result
			var bestSeconds float64
			for _, key := range []string{"primary_window", "secondary_window", "primaryWindow", "secondaryWindow"} {
				row := rate.Get(key)
				if !row.Exists() {
					continue
				}
				secs := firstGJSON(row, "limit_window_seconds", "limitWindowSeconds").Float()
				if !best.Exists() || secs > bestSeconds {
					best = row
					bestSeconds = secs
				}
			}
			used := firstGJSON(best, "used_percent", "usedPercent")
			if used.Exists() {
				var reset *time.Time
				seconds := firstGJSON(best, "reset_at", "resetAt").Int()
				if seconds == 0 {
					after := firstGJSON(best, "reset_after_seconds", "resetAfterSeconds").Int()
					if after > 0 {
						seconds = time.Now().Unix() + after
					}
				}
				if seconds > 0 {
					value := time.Unix(seconds, 0).UTC()
					reset = &value
				}
				hours := bestSeconds / 3600
				windowID := "weekly"
				if hours >= 28*24 {
					windowID = "monthly"
				} else if hours <= 24 {
					windowID = "daily"
				}
				appendWindow(windowID, 100-used.Float(), reset, &hours, nil)
			}
		case "kiro":
			subscription := firstGJSON(root, "subscriptionInfo", "SubscriptionInfo")
			plan = firstGJSON(subscription, "type", "subscriptionType", "subscription_type").String()
			for index, row := range firstGJSON(root, "usageBreakdownList", "UsageBreakdownList").Array() {
				resource := firstGJSON(row, "resourceType", "displayName").String()
				if resource == "" {
					resource = "AGENTIC_REQUEST"
				}
				add := func(kind string, value gjson.Result) {
					usage := firstGJSON(value, "currentUsageWithPrecision", "currentUsage", "used")
					limit := firstGJSON(value, "usageLimitWithPrecision", "usageLimit", "limit")
					if !limit.Exists() || limit.Float() <= 0 {
						return
					}
					cap := limit.Float()
					remaining := math.Max(0, math.Min(100, (cap-usage.Float())/cap*100))
					var reset *time.Time
					seconds := firstGJSON(value, "nextDateReset", "nextDateResetUnix").Float()
					if seconds > 0 {
						if seconds > 1e11 {
							seconds /= 1000
						}
						parsed := time.Unix(0, int64(seconds*float64(time.Second))).UTC()
						reset = &parsed
					}
					appendWindow(fmt.Sprintf("kiro-%s-%s-%d", kind, strings.ToLower(resource), index), remaining, reset, nil, &cap)
				}
				add("included", row)
				trial := firstGJSON(row, "freeTrialInfo", "FreeTrialInfo")
				if trial.Exists() {
					add("trial", trial)
				}
			}
		default:
			if provider == "zai" {
				plan = root.Get("data.level").String()
			}
			var windows []model.ProviderQuotaWindow
			var parseErr error
			switch provider {
			case "opencode-go":
				windows, parseErr = parseOpenCodeGoQuota(body)
			case "zai":
				windows, parseErr = parseZaiQuota(body, time.Now().UTC())
			}
			if parseErr != nil {
				return nil, parseErr
			}
			for _, window := range windows {
				if window.Percent == nil {
					continue
				}
				var capacity *float64
				if provider == "kiro" && window.Limit != nil {
					v := float64(*window.Limit)
					capacity = &v
				}
				var period *float64
				if window.Label == "weekly" {
					hours := 168.0
					period = &hours
				}
				appendWindow(window.Label, 100-*window.Percent, window.ResetsAt, period, capacity)
			}
		}
		return nil, nil
	}, true)
	return samples, err
}
