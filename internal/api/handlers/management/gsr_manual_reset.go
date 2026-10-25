package management

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const claudeResetGrantsQuotaURL = claudeUsageQuotaURL + "?cedar_ember=1&skip_spend=1"

// parseManualResetExpiry returns the earliest available credit that can reset
// the shared weekly allowance. Healthy Claude accounts may require hitting a
// limit before redeeming a grant, so that condition does not exclude a grant.
func parseManualResetExpiry(provider string, body []byte, now time.Time) (time.Time, error) {
	if !gjson.ValidBytes(body) {
		return time.Time{}, fmt.Errorf("decode %s manual reset response", provider)
	}
	root := gjson.ParseBytes(body)
	var credits []gjson.Result
	switch provider {
	case "codex":
		if !root.Get("credits").IsArray() {
			return time.Time{}, fmt.Errorf("Codex manual reset response has no credits array")
		}
		credits = root.Get("credits").Array()
	case "claude":
		root = root.Get("cedar_ember")
		if root.Get("eligible").Type != gjson.True && root.Get("eligible").Type != gjson.False {
			return time.Time{}, fmt.Errorf("Claude usage response has no reset grant eligibility")
		}
		if !root.Get("eligible").Bool() {
			return time.Time{}, nil
		}
		if !root.Get("grants").IsArray() {
			return time.Time{}, fmt.Errorf("Claude usage response has no reset grants array")
		}
		credits = root.Get("grants").Array()
	default:
		return time.Time{}, fmt.Errorf("provider %q has no manual reset probe", provider)
	}

	var earliest time.Time
	for _, credit := range credits {
		var expiresAt time.Time
		switch provider {
		case "codex":
			resetType := cmp.Or(credit.Get("reset_type").String(), credit.Get("resetType").String())
			if resetType != "codex_rate_limits" || credit.Get("status").String() != "available" {
				continue
			}
			expiresAt = quotaReserveResetFromValue(credit.Get("expires_at"))
			if expiresAt.IsZero() {
				expiresAt = quotaReserveResetFromValue(credit.Get("expiresAt"))
			}
		case "claude":
			if credit.Get("resets_left").Float() <= 0 || credit.Get("paused").Bool() {
				continue
			}
			requiresLimit := !credit.Get("use_requires_limit").Exists() || credit.Get("use_requires_limit").Bool()
			if !credit.Get("usable_now").Bool() && (!requiresLimit || root.Get("at_limit").Bool()) {
				continue
			}
			clearsWeekly := false
			for _, window := range credit.Get("clears").Array() {
				if window.String() == "seven_day" {
					clearsWeekly = true
				}
			}
			if !clearsWeekly {
				continue
			}
			if start := credit.Get("starts_at"); start.Exists() && start.Type != gjson.Null {
				startsAt, errParse := time.Parse(time.RFC3339, start.String())
				if errParse != nil || startsAt.After(now) {
					continue
				}
			}
			expiresAt, _ = time.Parse(time.RFC3339, strings.TrimSpace(credit.Get("ends_at").String()))
		}
		if !expiresAt.After(now) {
			continue
		}
		if provider == "claude" {
			if cooldown, errParse := time.Parse(time.RFC3339, root.Get("cooldown_until").String()); errParse == nil && !expiresAt.After(cooldown) {
				continue
			}
		}
		if earliest.IsZero() || expiresAt.Before(earliest) {
			earliest = expiresAt
		}
	}
	return earliest, nil
}
