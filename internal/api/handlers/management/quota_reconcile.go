package management

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func firstQuotaValue(root ojson.Result, paths ...string) ojson.Result {
	for _, path := range paths {
		if value := root.Get(path); value.Exists() && value.Type != ojson.Null {
			return value
		}
	}
	return ojson.Result{}
}

func codexRateLimitDecision(raw json.RawMessage) claudeUsageDecision {
	if codexRateLimitFamilyHealthy(raw) {
		return claudeUsageDecision{actionable: true}
	}
	if !ojson.ValidBytes(raw) {
		return claudeUsageDecision{}
	}
	root := ojson.ParseBytes(raw)
	var reset time.Time
	for _, pair := range [][]string{{"primary_window", "primaryWindow"}, {"secondary_window", "secondaryWindow"}} {
		window := firstQuotaValue(root, pair...)
		value := firstQuotaValue(window, "used_percent", "usedPercent")
		percent, errParse := strconv.ParseFloat(value.String(), 64)
		if errParse != nil || math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 {
			continue
		}
		if percent < 100 {
			continue
		}
		var parsed codexUsageWindowResponse
		if json.Unmarshal([]byte(window.Raw), &parsed) != nil {
			return claudeUsageDecision{}
		}
		windowReset := parseCodexUsageWindowReset(parsed)
		if !windowReset.After(time.Now()) {
			return claudeUsageDecision{}
		}
		if reset.IsZero() || windowReset.Before(reset) {
			reset = windowReset
		}
	}
	return claudeUsageDecision{actionable: !reset.IsZero(), exhausted: true, resetAt: reset}
}

// syncQuotaRead applies only fresh upstream observations. Keeping this at the
// transport boundary covers management reads, analytics and routing probes.
func (h *Handler) syncQuotaRead(ctx context.Context, auth *coreauth.Auth, target *url.URL, body []byte) {
	h.syncUsageProbeCooldown(ctx, auth, target, body)
	h.syncCodexUsageCooldown(ctx, auth, target, body)
	h.syncClaudeUsageCooldown(ctx, auth, target, body)
	h.applyQuotaReserveFromQuotaRead(ctx, auth, target, body)
	if h.authManager == nil || auth == nil || !ojson.ValidBytes(body) {
		return
	}
	root := ojson.ParseBytes(body)
	var bars []coreauth.QuotaReserveBar
	var reportedModelIDs []string
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "antigravity":
		if isAntigravityQuotaURL(target) {
			for _, group := range root.Get("groups").Array() {
				groupBars := make([]coreauth.QuotaReserveBar, 0)
				families := make(map[string]bool)
				valid := true
				for _, bucket := range group.Get("buckets").Array() {
					value := firstQuotaValue(bucket, "remainingFraction", "remaining_fraction")
					fraction, ok := parseNumericFraction(value)
					if !ok || fraction < 0 || fraction > 1 {
						valid = false
						break
					}
					id := strings.ToLower(firstQuotaValue(bucket, "bucketId", "bucket_id").String())
					for _, family := range []string{"gemini", "claude", "gpt"} {
						if strings.HasPrefix(id, family+"-") {
							families[family] = true
						}
					}
					groupBars = append(groupBars, coreauth.QuotaReserveBar{RemainingPercent: fraction * 100, ResetAt: quotaResetTime(firstQuotaValue(bucket, "resetTime", "reset_time"))})
				}
				if !valid {
					continue
				}
				label := strings.ToLower(firstQuotaValue(group, "displayName", "display_name").String())
				if label == "gemini models" {
					families["gemini"] = true
				} else if label == "claude and gpt models" {
					families["claude"], families["gpt"] = true, true
				}
				models := h.quotaModelsForPool(auth, func(id string) bool {
					return families[strings.SplitN(strings.ToLower(id), "-", 2)[0]]
				})
				if len(models) > 0 {
					if _, errApply := h.authManager.ApplyProviderQuotaBarsForModels(ctx, auth.ID, groupBars, models); errApply != nil {
						log.WithError(errApply).WithField("auth_id", auth.ID).Debug("failed to sync Antigravity quota pool")
					}
				}
			}
		}
	case "kiro":
		if target.Path == "/getUsageLimits" {
			// Trial and paid credits are alternatives. Any remaining pool makes
			// this credential usable; an exhausted trial alone cannot block it.
			parsed, errParse := kiroQuotaReserveBars(body)
			if errParse != nil {
				return
			}
			for _, bar := range parsed {
				if bar.RemainingPercent > 0 {
					bars = []coreauth.QuotaReserveBar{bar}
					break
				}
			}
			if len(bars) == 0 {
				bars = parsed
			}
		}
	case "kimi":
		if exactQuotaURL(target, kimiUsageQuotaURL, kimiAIUsageQuotaURL) {
			add := func(value ojson.Result) {
				limit, used, remaining := value.Get("limit"), value.Get("used"), value.Get("remaining")
				limitNumber, validLimit := parseNumericFraction(limit)
				usedNumber, validUsed := parseNumericFraction(used)
				remainingNumber, validRemaining := parseNumericFraction(remaining)
				if !validLimit || limitNumber <= 0 || (!validUsed && !validRemaining) {
					return
				}
				percent := 100 * (limitNumber - usedNumber) / limitNumber
				if validRemaining {
					percent = 100 * remainingNumber / limitNumber
				}
				reset := quotaResetTime(firstQuotaValue(value, "resetAt", "reset_at", "resetTime", "reset_time"))
				if reset.IsZero() {
					if seconds := firstQuotaValue(value, "resetIn", "reset_in", "ttl").Float(); seconds > 0 {
						reset = time.Now().Add(time.Duration(seconds * float64(time.Second)))
					}
				}
				bars = append(bars, coreauth.QuotaReserveBar{RemainingPercent: math.Max(0, percent), ResetAt: reset})
			}
			add(root.Get("usage"))
			for _, item := range root.Get("limits").Array() {
				if detail := item.Get("detail"); detail.IsObject() {
					add(detail)
				} else {
					add(item)
				}
			}
			if monthly := root.Get("usages.limit_month_total"); monthly.Get("used_ratio").Exists() {
				if ratio, ok := parseNumericFraction(monthly.Get("used_ratio")); ok && ratio >= 0 {
					bars = append(bars, coreauth.QuotaReserveBar{RemainingPercent: math.Max(0, 100*(1-ratio)), ResetAt: quotaResetTime(monthly.Get("reset_time"))})
				}
			}
		}
	case "xai":
		if exactQuotaURL(target, xaiHealthURL) {
			choices := root.Get("choices")
			if root.Get("object").String() == "chat.completion" && choices.IsArray() && len(choices.Array()) > 0 && root.Get("error").Type == ojson.Null {
				// A paid health probe proves only its requested model allowance.
				bars = []coreauth.QuotaReserveBar{{RemainingPercent: 100}}
				reportedModelIDs = h.quotaModelsForPool(auth, func(id string) bool { return strings.EqualFold(id, xaiHealthModel) })
			}
		} else if exactQuotaURL(target, xaiWeeklyQuotaURL, xaiMonthlyQuotaURL) {
			config := root.Get("config")
			percent := firstQuotaValue(config, "creditUsagePercent", "credit_usage_percent")
			if used, ok := parseNumericFraction(percent); ok && used >= 0 {
				bars = append(bars, coreauth.QuotaReserveBar{RemainingPercent: math.Max(0, 100-used), ResetAt: quotaResetTime(firstQuotaValue(config, "currentPeriod.end", "current_period.end", "billingPeriodEnd", "billing_period_end"))})
			}
			// Monthly billing caps can be supplemented by on-demand or prepaid
			// credit, so only an explicit remaining balance proves recovery.
			if balance, ok := parseNumericFraction(firstQuotaValue(config, "prepaidBalance", "prepaid_balance")); ok && balance > 0 {
				bars = []coreauth.QuotaReserveBar{{RemainingPercent: 100}}
			}
		}
	}
	if _, errApply := h.authManager.ApplyProviderQuotaBarsForModels(ctx, auth.ID, bars, reportedModelIDs); errApply != nil {
		log.WithError(errApply).WithField("auth_id", auth.ID).Debug("failed to sync provider quota cooldown")
	}
}

func quotaResetTime(value ojson.Result) time.Time {
	if reset, errParse := time.Parse(time.RFC3339, strings.TrimSpace(value.String())); errParse == nil {
		return reset
	}
	seconds, errParse := strconv.ParseFloat(value.String(), 64)
	if errParse != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return time.Time{}
	}
	if seconds > 1e11 {
		seconds /= 1000
	}
	return time.Unix(int64(seconds), 0)
}

// quotaModelsForPool classifies upstream identities and returns the routing
// cooldown keys, including existing states from previously registered aliases.
func (h *Handler) quotaModelsForPool(auth *coreauth.Auth, match func(string) bool) []string {
	if h == nil || h.authManager == nil || auth == nil {
		return []string{}
	}
	routes := make([]string, 0)
	existing := make(map[string]bool)
	for _, model := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
		if model != nil {
			routes = append(routes, model.ID)
		}
	}
	if current, ok := h.authManager.GetByID(auth.ID); ok {
		for key := range current.ModelStates {
			routes = append(routes, key)
			existing[key] = true
		}
	}
	keys := make([]string, 0)
	seen := make(map[string]bool)
	for _, route := range routes {
		upstream, resolved := h.authManager.ProviderQuotaModel(auth, route)
		if !match(upstream) {
			continue
		}
		if existing[route] {
			resolved = append(resolved, route)
		}
		for _, key := range resolved {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	return keys
}
