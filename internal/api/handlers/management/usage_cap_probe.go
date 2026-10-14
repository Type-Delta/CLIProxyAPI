package management

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// Usage-cap probes read the same usage endpoints, with the same headers, as the
// CPAMC quota adapters. Matching requests share one quota cache entry, so a cap
// check right after the quota page loaded costs no provider call. Bar IDs and
// remaining-percent math mirror the adapters so a cap set on a rendered bar
// matches the bar evaluated here.
//
// Keep these headers in sync with web/management-center/src/utils/quota/constants.ts
// and the Z.ai / OpenCode Go / Kiro adapters.
var (
	usageCapClaudeHeaders = map[string]string{
		"User-Agent":     "claude-cli/2.1.280 (external, cli)",
		"Authorization":  "Bearer $TOKEN$",
		"Content-Type":   "application/json",
		"anthropic-beta": "oauth-2025-04-20",
	}
	usageCapCodexHeaders = map[string]string{
		"Authorization": "Bearer $TOKEN$",
		"Content-Type":  "application/json",
		"User-Agent":    "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)",
	}
	usageCapBearerJSONHeaders = map[string]string{
		"Authorization": "Bearer $TOKEN$",
		"Accept":        "application/json",
	}
)

// UsageCapBars is the coreauth.UsageCapProber for supported providers.
func (h *Handler) UsageCapBars(ctx context.Context, auth *coreauth.Auth) ([]coreauth.UsageCapBar, error) {
	if h == nil || auth == nil {
		return nil, fmt.Errorf("usage cap probe unavailable")
	}
	switch coreauth.QuotaRecoveryProviderForAuth(auth) {
	case "claude":
		body, err := h.usageCapFetch(ctx, auth, claudeUsageQuotaURL, usageCapClaudeHeaders)
		if err != nil {
			return nil, err
		}
		return claudeUsageCapBars(body)
	case "codex":
		headers := cloneStringMap(usageCapCodexHeaders)
		if accountID := codexUsageCapAccountID(auth); accountID != "" {
			headers["Chatgpt-Account-Id"] = accountID
		}
		body, err := h.usageCapFetch(ctx, auth, codexUsageQuotaURL, headers)
		if err != nil {
			return nil, err
		}
		return codexUsageCapBars(body)
	case "zai":
		body, err := h.usageCapFetch(ctx, auth, zaiUsageQuotaURL, usageCapBearerJSONHeaders)
		if err != nil {
			return nil, err
		}
		return zaiUsageCapBars(body)
	case "opencode-go":
		body, err := h.usageCapFetch(ctx, auth, opencodeGoUsageQuotaURL, usageCapBearerJSONHeaders)
		if err != nil {
			return nil, err
		}
		return opencodeGoUsageCapBars(body)
	case "kiro":
		body, err := h.usageCapFetch(ctx, auth, kiroUsageCapURL(auth), usageCapBearerJSONHeaders)
		if err != nil {
			return nil, err
		}
		return kiroUsageCapBars(body)
	}
	return nil, fmt.Errorf("provider %q has no usage cap probe", auth.Provider)
}

func (h *Handler) usageCapFetch(ctx context.Context, auth *coreauth.Auth, targetURL string, headers map[string]string) ([]byte, error) {
	parsedURL, errParse := url.Parse(targetURL)
	if errParse != nil || parsedURL.Host == "" {
		return nil, fmt.Errorf("invalid usage URL")
	}
	headers = cloneStringMap(headers)
	key := buildQuotaCacheKey(h, auth, http.MethodGet, parsedURL, "", headers, "")
	outcome := h.getQuotaCache().do(ctx, key, func(requestContext context.Context) quotaCallOutcome {
		return h.executeAPICall(requestContext, http.MethodGet, targetURL, "", auth, "", headers, "")
	})
	if outcome.outerError != "" {
		return nil, fmt.Errorf("usage request failed: %s", outcome.outerError)
	}
	if !outcome.successfulResponse() {
		return nil, fmt.Errorf("usage request returned status %d", outcome.response.StatusCode)
	}
	return []byte(outcome.response.Body), nil
}

// applyUsageCapsFromQuotaRead evaluates caps from a management quota read so a
// quota-page refresh updates routing without waiting for the next request.
func (h *Handler) applyUsageCapsFromQuotaRead(auth *coreauth.Auth, parsedURL *url.URL, body []byte) {
	if h == nil || h.authManager == nil || auth == nil || parsedURL == nil || len(coreauth.UsageCapsForAuth(auth)) == 0 {
		return
	}
	var (
		bars []coreauth.UsageCapBar
		err  error
	)
	switch coreauth.QuotaRecoveryProviderForAuth(auth) {
	case "claude":
		if !exactQuotaURL(parsedURL, claudeUsageQuotaURL) {
			return
		}
		bars, err = claudeUsageCapBars(body)
	case "codex":
		if !exactQuotaURL(parsedURL, codexUsageQuotaURL) {
			return
		}
		bars, err = codexUsageCapBars(body)
	case "zai":
		if !exactQuotaURL(parsedURL, zaiUsageQuotaURL) {
			return
		}
		bars, err = zaiUsageCapBars(body)
	case "opencode-go":
		if !exactQuotaURL(parsedURL, opencodeGoUsageQuotaURL) {
			return
		}
		bars, err = opencodeGoUsageCapBars(body)
	case "kiro":
		if parsedURL.Host == "" || !strings.HasSuffix(parsedURL.Path, "/getUsageLimits") {
			return
		}
		bars, err = kiroUsageCapBars(body)
	default:
		return
	}
	if err == nil {
		h.authManager.ApplyUsageCapBars(auth, bars)
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func clampRemaining(value float64) float64 {
	return math.Max(0, math.Min(100, value))
}

func usageCapResetFromValue(value gjson.Result) time.Time {
	if !value.Exists() || value.Type == gjson.Null {
		return time.Time{}
	}
	if value.Type == gjson.Number {
		number := value.Float()
		if number <= 0 {
			return time.Time{}
		}
		if number < 1e11 {
			return time.Unix(0, int64(number*float64(time.Second))).UTC()
		}
		return time.UnixMilli(int64(number)).UTC()
	}
	text := strings.TrimSpace(value.String())
	if text == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339, text); err == nil {
		return parsed.UTC()
	}
	return time.Time{}
}

// claudeUsageCapBars mirrors buildClaudeQuotaWindows: one bar per named window
// plus the Fable weekly_scoped limit.
func claudeUsageCapBars(body []byte) ([]coreauth.UsageCapBar, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("decode Claude usage response: invalid JSON")
	}
	root := gjson.ParseBytes(body)
	var fable gjson.Result
	for _, limit := range root.Get("limits").Array() {
		model := strings.ToLower(strings.TrimSpace(limit.Get("scope.model.display_name").String()))
		if !strings.EqualFold(strings.TrimSpace(limit.Get("kind").String()), "weekly_scoped") ||
			(model != "fable" && model != "fable 5") || limit.Get("percent").Type != gjson.Number {
			continue
		}
		if limit.Get("is_active").Bool() {
			fable = limit
			break
		}
		if !fable.Exists() {
			fable = limit
		}
	}
	windows := []struct{ key, id string }{
		{"five_hour", "five-hour"},
		{"seven_day", "seven-day"},
		{"seven_day_oauth_apps", "seven-day-oauth-apps"},
		{"seven_day_opus", "seven-day-opus"},
		{"seven_day_sonnet", "seven-day-sonnet"},
		{"seven_day_cowork", "seven-day-cowork"},
		{"iguana_necktie", "seven-day-fable"},
	}
	bars := make([]coreauth.UsageCapBar, 0, len(windows))
	for _, window := range windows {
		if window.key == "iguana_necktie" && fable.Exists() {
			continue
		}
		value := root.Get(window.key)
		utilization := value.Get("utilization")
		if !value.IsObject() || utilization.Type != gjson.Number {
			continue
		}
		bars = append(bars, coreauth.UsageCapBar{
			ID:               window.id,
			RemainingPercent: clampRemaining(100 - utilization.Float()),
			ResetAt:          usageCapResetFromValue(value.Get("resets_at")),
		})
	}
	if fable.Exists() {
		bars = append(bars, coreauth.UsageCapBar{
			ID:               "seven-day-fable",
			RemainingPercent: clampRemaining(100 - fable.Get("percent").Float()),
			ResetAt:          usageCapResetFromValue(fable.Get("resets_at")),
		})
	}
	return bars, nil
}

var usageCapNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// codexUsageCapBars mirrors buildCodexQuotaWindows: five-hour and weekly or
// monthly bars for the standard, code-review, and each additional allowance.
func codexUsageCapBars(body []byte) ([]coreauth.UsageCapBar, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("decode Codex usage response: invalid JSON")
	}
	root := gjson.ParseBytes(body)
	const fiveHourSeconds = 18000
	const weekSeconds = 604800
	const minMonthSeconds = 28 * 24 * 60 * 60
	const maxMonthSeconds = 31 * 24 * 60 * 60
	now := time.Now()
	bars := make([]coreauth.UsageCapBar, 0, 6)

	seconds := func(window gjson.Result) (float64, bool) {
		value := firstGJSON(window, "limit_window_seconds", "limitWindowSeconds")
		if value.Type != gjson.Number {
			return 0, false
		}
		return value.Float(), true
	}
	isMonthly := func(window gjson.Result) bool {
		value, ok := seconds(window)
		return ok && value >= minMonthSeconds && value <= maxMonthSeconds
	}
	// classify returns indexes into [primary, secondary] (-1 when absent),
	// matching CPAMC's pickClassifiedWindows including its order fallback.
	classify := func(limit gjson.Result) (windows [2]gjson.Result, fiveHour, weekly int) {
		windows = [2]gjson.Result{firstGJSON(limit, "primary_window", "primaryWindow"), firstGJSON(limit, "secondary_window", "secondaryWindow")}
		fiveHour, weekly = -1, -1
		for index, window := range windows {
			if !window.IsObject() {
				continue
			}
			value, ok := seconds(window)
			if ok && value == fiveHourSeconds && fiveHour < 0 {
				fiveHour = index
			} else if ok && (value == weekSeconds || isMonthly(window)) && weekly < 0 {
				weekly = index
			}
		}
		if fiveHour < 0 && windows[0].IsObject() && weekly != 0 {
			fiveHour = 0
		}
		if weekly < 0 && windows[1].IsObject() && fiveHour != 1 {
			weekly = 1
		}
		return windows, fiveHour, weekly
	}
	add := func(id string, window, limit gjson.Result) {
		if !window.IsObject() {
			return
		}
		used := firstGJSON(window, "used_percent", "usedPercent")
		reset := usageCapResetFromValue(firstGJSON(window, "reset_at", "resetAt"))
		if reset.IsZero() {
			if after := firstGJSON(window, "reset_after_seconds", "resetAfterSeconds"); after.Type == gjson.Number && after.Float() >= 0 {
				reset = now.Add(time.Duration(after.Float() * float64(time.Second))).UTC()
			}
		}
		remaining := 0.0
		if used.Type == gjson.Number {
			remaining = clampRemaining(100 - used.Float())
		} else {
			reached := firstGJSON(limit, "limit_reached", "limitReached").Bool() || firstGJSON(limit, "allowed").Type == gjson.False
			if !reached || reset.IsZero() {
				return
			}
		}
		bars = append(bars, coreauth.UsageCapBar{ID: id, RemainingPercent: remaining, ResetAt: reset})
	}
	family := func(limit gjson.Result, fiveHourID, weeklyID, monthlyID string) {
		if !limit.IsObject() {
			return
		}
		windows, fiveHour, weekly := classify(limit)
		if fiveHour >= 0 {
			add(fiveHourID, windows[fiveHour], limit)
		}
		if weekly >= 0 {
			if isMonthly(windows[weekly]) {
				add(monthlyID, windows[weekly], limit)
			} else {
				add(weeklyID, windows[weekly], limit)
			}
		}
	}
	family(firstGJSON(root, "rate_limit", "rateLimit"), "five-hour", "weekly", "monthly")
	family(firstGJSON(root, "code_review_rate_limit", "codeReviewRateLimit"), "code-review-five-hour", "code-review-weekly", "code-review-monthly")
	for index, item := range firstGJSON(root, "additional_rate_limits", "additionalRateLimits").Array() {
		limit := firstGJSON(item, "rate_limit", "rateLimit")
		if !limit.IsObject() {
			continue
		}
		name := strings.TrimSpace(firstGJSON(item, "limit_name", "limitName").String())
		if name == "" {
			name = strings.TrimSpace(firstGJSON(item, "metered_feature", "meteredFeature").String())
		}
		prefix := strings.Trim(usageCapNonAlnum.ReplaceAllString(strings.ToLower(name), "-"), "-")
		if prefix == "" {
			prefix = fmt.Sprintf("additional-%d", index+1)
		}
		family(limit,
			fmt.Sprintf("%s-five-hour-%d", prefix, index),
			fmt.Sprintf("%s-weekly-%d", prefix, index),
			fmt.Sprintf("%s-monthly-%d", prefix, index))
	}
	return bars, nil
}

// zaiUsageCapBars mirrors buildZaiQuotaWindows.
func zaiUsageCapBars(body []byte) ([]coreauth.UsageCapBar, error) {
	var response zaiQuotaResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode Z.ai quota response: %w", err)
	}
	if response.Code != http.StatusOK || !response.Success {
		return nil, fmt.Errorf("Z.ai quota response was unsuccessful")
	}
	bars := make([]coreauth.UsageCapBar, 0, len(response.Data.Limits))
	for index, limit := range response.Data.Limits {
		id := fmt.Sprintf("zai-window-%d", index)
		switch limit.Unit {
		case 3:
			id = fmt.Sprintf("zai-hour-%d", limit.Number)
		case 6:
			id = fmt.Sprintf("zai-week-%d", limit.Number)
		}
		bar := coreauth.UsageCapBar{ID: id, RemainingPercent: clampRemaining(100 - limit.Percentage)}
		if limit.NextResetTime > 0 {
			bar.ResetAt = time.UnixMilli(limit.NextResetTime).UTC()
		}
		bars = append(bars, bar)
	}
	return bars, nil
}

// opencodeGoUsageCapBars mirrors buildOpenCodeGoQuotaWindows; a non-ok status
// counts as exhausted.
func opencodeGoUsageCapBars(body []byte) ([]coreauth.UsageCapBar, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("decode OpenCode Go quota response: invalid JSON")
	}
	usage := gjson.GetBytes(body, "usage")
	bars := make([]coreauth.UsageCapBar, 0, 3)
	for _, key := range []string{"rolling", "weekly", "monthly"} {
		window := usage.Get(key)
		if !window.IsObject() {
			continue
		}
		percent := window.Get("percent")
		status := strings.ToLower(strings.TrimSpace(window.Get("status").String()))
		exhausted := status != "" && status != "ok"
		if percent.Type != gjson.Number && !exhausted {
			continue
		}
		remaining := clampRemaining(100 - percent.Float())
		if exhausted {
			remaining = 0
		}
		bars = append(bars, coreauth.UsageCapBar{
			ID:               "opencode-go-" + key,
			RemainingPercent: remaining,
			ResetAt:          usageCapResetFromValue(window.Get("resetsAt")),
		})
	}
	return bars, nil
}

// kiroUsageCapURL matches CPAMC's buildKiroUsageUrl, which always targets
// us-east-1 and adds profileArn when the credential has one.
func kiroUsageCapURL(auth *coreauth.Auth) string {
	parsed, _ := url.Parse("https://codewhisperer.us-east-1.amazonaws.com/getUsageLimits?origin=AI_EDITOR&resourceType=AGENTIC_REQUEST&isEmailRequired=true")
	if profileARN := kiroAuthValue(auth, "profile_arn"); profileARN != "" {
		query := parsed.Query()
		query.Set("profileArn", profileARN)
		parsed.RawQuery = query.Encode()
	}
	return parsed.String()
}

// kiroUsageCapBars mirrors parseKiroQuotaPayload and KiroQuotaBody's remaining percent.
func kiroUsageCapBars(body []byte) ([]coreauth.UsageCapBar, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("decode Kiro usage response: invalid JSON")
	}
	root := gjson.ParseBytes(body)
	rows := firstGJSON(root, "usageBreakdownList", "UsageBreakdownList")
	if !rows.IsArray() {
		return nil, fmt.Errorf("Kiro usage response has no usage breakdown")
	}
	bars := make([]coreauth.UsageCapBar, 0)
	build := func(record gjson.Result, index int, trial bool) {
		used := firstGJSON(record, "currentUsageWithPrecision", "currentUsage", "used")
		limit := firstGJSON(record, "usageLimitWithPrecision", "usageLimit", "limit")
		if !used.Exists() && !limit.Exists() {
			return
		}
		if !limit.Exists() {
			return
		}
		resourceType := strings.TrimSpace(firstGJSON(record, "resourceType", "displayName").String())
		if resourceType == "" {
			resourceType = "AGENTIC_REQUEST"
		}
		kind := "included"
		if trial {
			kind = "trial"
		}
		usage := math.Max(0, used.Float())
		total := math.Max(0, limit.Float())
		remaining := 0.0
		if total > 0 {
			remaining = clampRemaining((total - usage) / total * 100)
		} else if usage <= 0 {
			return
		}
		reset := firstGJSON(record, "nextDateReset", "nextDateResetUnix")
		bars = append(bars, coreauth.UsageCapBar{
			ID:               fmt.Sprintf("kiro-%s-%s-%d", kind, strings.ToLower(resourceType), index),
			RemainingPercent: remaining,
			ResetAt:          usageCapResetFromValue(reset),
		})
	}
	for index, row := range rows.Array() {
		build(row, index, false)
		build(firstGJSON(row, "freeTrialInfo", "FreeTrialInfo"), index, true)
	}
	return bars, nil
}

// codexUsageCapAccountID matches CPAMC's resolveCodexChatgptAccountId for the
// fields the auth-files listing exposes.
func codexUsageCapAccountID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	for _, key := range []string{"chatgpt_account_id", "chatgptAccountId"} {
		if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
		if value := strings.TrimSpace(authAttribute(auth, key)); value != "" {
			return value
		}
	}
	if claims := extractCodexIDTokenClaims(auth); claims != nil {
		if value, ok := claims["chatgpt_account_id"].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
