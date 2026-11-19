package auth

import (
	"math"
	"strconv"
	"strings"
	"time"
)

const gsrQuotaSignalPrefix = "gsr-quota:"

// clearGSRQuotaObservation invalidates a quota snapshot so the next pick can
// refresh it. Cooldown has its own state and is reconciled separately.
func clearGSRQuotaObservation(auth *Auth) bool {
	if QuotaRecoveryProviderForAuth(auth) == "" {
		return false
	}
	changed := auth.Quota.ClearObservationSignals()
	if auth.Quota.ManualReset.ObservedAt.IsZero() {
		return changed
	}
	auth.Quota.ManualReset = ManualResetObservation{}
	return true
}

// SetGSRQuotaWindows replaces the probed windows in this quota snapshot.
// Each utilization sample is paired with its own expiry.
func (q *QuotaState) SetGSRQuotaWindows(provider string, bars []QuotaReserveBar) {
	if q.Signals == nil {
		q.Signals = make(map[string]string)
	}
	for key := range q.Signals {
		if strings.HasPrefix(key, gsrQuotaSignalPrefix) {
			delete(q.Signals, key)
		}
	}
	added := 0
	for _, bar := range bars {
		if bar.ResetAt.IsZero() || !(bar.RemainingPercent >= 0 && bar.RemainingPercent <= 100) || len(bar.ID) > maxQuotaSignalValue {
			continue
		}
		q.Signals[gsrQuotaSignalPrefix+provider+":"+bar.ID] = strconv.FormatFloat(bar.RemainingPercent, 'f', -1, 64) +
			":" + strconv.FormatInt(bar.ResetAt.UnixMilli(), 10)
		added++
		if added == maxQuotaSignalHeaders {
			break
		}
	}
}

// gsrRemainingFraction uses the tightest applicable window as the estimate
// of how much quota is available before the request hits a limit.
func gsrRemainingFraction(auth *Auth, provider, model string, now time.Time) (float64, bool) {
	if auth.Quota.ObservedAt.After(now) {
		return 0, false
	}
	signals := auth.Quota.Signals
	model = strings.ToLower(canonicalModelKey(model))
	fable := strings.Contains(model, "fable")
	spark := strings.HasSuffix(model, "-spark")
	review := model == "codex-auto-review"
	remaining, observed := 1.0, false
	add := func(fraction float64) {
		remaining = math.Min(remaining, fraction)
		observed = true
	}
	prefix := gsrQuotaSignalPrefix + provider + ":"
	for key, raw := range signals {
		id, ok := strings.CutPrefix(key, prefix)
		if !ok {
			continue
		}
		switch provider {
		case "codex":
			switch {
			case spark:
				if !strings.HasPrefix(id, "gpt-5-3-codex-spark-") && !strings.HasPrefix(id, "codex-bengalfox-") && !strings.HasPrefix(id, "bengalfox-") {
					continue
				}
			case review:
				if !strings.HasPrefix(id, "code-review-") {
					continue
				}
			default:
				if id != "five-hour" && id != "weekly" && id != "monthly" {
					continue
				}
			}
		case "claude":
			switch {
			case id == "five-hour", id == "seven-day", id == "seven-day-fable" && fable,
				id == "seven-day-opus" && strings.Contains(model, "opus"),
				id == "seven-day-sonnet" && strings.Contains(model, "sonnet"):
			default:
				continue
			}
		}
		percent, reset, ok := strings.Cut(raw, ":")
		value, errPercent := strconv.ParseFloat(percent, 64)
		ms, errReset := strconv.ParseInt(reset, 10, 64)
		if ok && errPercent == nil && value >= 0 && value <= 100 && errReset == nil && time.UnixMilli(ms).After(now) {
			add(value / 100)
		}
	}
	if observed {
		return remaining, true
	}

	signal := func(name string) string {
		if value, ok := signals[name]; ok {
			return value
		}
		for key, value := range signals {
			if strings.EqualFold(key, name) {
				return value
			}
		}
		return ""
	}
	switch provider {
	case "codex":
		families := []string{"X-Codex-"}
		if review {
			families = []string{"X-Codex-Code-Review-"}
		} else if spark {
			families = []string{
				"X-Codex-Bengalfox-", "X-Codex-Additional-Gpt-5-3-Codex-Spark-", "X-Codex-Additional-Codex-Bengalfox-",
			}
		}
		for _, family := range families {
			for _, window := range []string{"Primary-", "Secondary-"} {
				name := family + window
				used, errUsed := strconv.ParseFloat(strings.TrimSpace(signal(name+"Used-Percent")), 64)
				if errUsed != nil || !(used >= 0) || math.IsInf(used, 1) {
					continue
				}
				reset, ok := parseRecoverySignalTime(signal(name+"Reset-At"), recoverySignalAbsolute, auth.Quota.ObservedAt)
				if !ok {
					reset, _ = parseRecoverySignalTime(signal(name+"Reset-After-Seconds"), recoverySignalAfterSeconds, auth.Quota.ObservedAt)
				}
				if reset.After(now) {
					add(max(0, (100-used)/100))
				}
			}
		}
	case "claude":
		windows := []string{"5h", "7d"}
		switch {
		case fable:
			windows = append(windows, "7d_oi")
		case strings.Contains(model, "opus"):
			windows = append(windows, "7d_opus")
		case strings.Contains(model, "sonnet"):
			windows = append(windows, "7d_sonnet")
		}
		for _, window := range windows {
			name := "Anthropic-Ratelimit-Unified-" + window
			used, errUsed := strconv.ParseFloat(strings.TrimSpace(signal(name+"-Utilization")), 64)
			if errUsed != nil || !(used >= 0) || math.IsInf(used, 1) {
				continue
			}
			reset, _ := parseRecoverySignalTime(signal(name+"-Reset"), recoverySignalAbsolute, auth.Quota.ObservedAt)
			if reset.After(now) {
				add(max(0, 1-used))
			}
		}
	}
	return remaining, observed
}
