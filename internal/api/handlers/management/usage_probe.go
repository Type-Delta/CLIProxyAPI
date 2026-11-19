package management

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const zaiUsageQuotaURL = "https://api.z.ai/api/monitor/usage/quota/limit"

const usageProbeMaxConcurrency = 8

var kiroUsageLimitsEndpoint = func(region string) string {
	return fmt.Sprintf("https://codewhisperer.%s.amazonaws.com", strings.TrimSpace(region))
}

type zaiQuotaResponse struct {
	Code    int  `json:"code"`
	Success bool `json:"success"`
	Data    struct {
		Limits []struct {
			Type          string   `json:"type"`
			Unit          int      `json:"unit"`
			Number        int      `json:"number"`
			Usage         *int64   `json:"usage"`
			CurrentValue  *int64   `json:"currentValue"`
			Remaining     *int64   `json:"remaining"`
			Percentage    *float64 `json:"percentage"`
			NextResetTime int64    `json:"nextResetTime"`
		} `json:"limits"`
	} `json:"data"`
}

type opencodeGoQuotaResponse struct {
	Usage json.RawMessage `json:"usage"`
}

type opencodeGoQuotaWindowResponse struct {
	Percent   *float64 `json:"percent"`
	ResetsAt  *string  `json:"resetsAt"`
	Limit     *int64   `json:"limit"`
	Used      *int64   `json:"used"`
	Remaining *int64   `json:"remaining"`
}

type codexRateLimitResponse struct {
	Allowed              *bool           `json:"allowed"`
	LimitReached         *bool           `json:"limit_reached"`
	LimitReachedCamel    *bool           `json:"limitReached"`
	PrimaryWindow        json.RawMessage `json:"primary_window"`
	PrimaryWindowCamel   json.RawMessage `json:"primaryWindow"`
	SecondaryWindow      json.RawMessage `json:"secondary_window"`
	SecondaryWindowCamel json.RawMessage `json:"secondaryWindow"`
}

type codexUsageWindowResponse struct {
	UsedPercent      json.RawMessage `json:"used_percent"`
	UsedPercentCamel json.RawMessage `json:"usedPercent"`
	WindowMinutes    json.RawMessage `json:"window_minutes"`
	WindowMinutesCam json.RawMessage `json:"windowMinutes"`
	WindowSeconds    json.RawMessage `json:"limit_window_seconds"`
	WindowSecondsCam json.RawMessage `json:"limitWindowSeconds"`
	ResetAt          json.RawMessage `json:"reset_at"`
	ResetAtCamel     json.RawMessage `json:"resetAt"`
	ResetAfter       json.RawMessage `json:"reset_after_seconds"`
	ResetAfterCamel  json.RawMessage `json:"resetAfterSeconds"`
}

type claudeUsageWindowResponse struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type claudeUsageLimitResponse struct {
	Kind     string   `json:"kind"`
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resets_at"`
	IsActive bool     `json:"is_active"`
	Scope    struct {
		Model struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

type claudeUsageResponse struct {
	FiveHour      *claudeUsageWindowResponse `json:"five_hour"`
	SevenDay      *claudeUsageWindowResponse `json:"seven_day"`
	IguanaNecktie *claudeUsageWindowResponse `json:"iguana_necktie"`
	Limits        []claudeUsageLimitResponse `json:"limits"`
}

func isClaudeFableScope(kind, model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.EqualFold(strings.TrimSpace(kind), "weekly_scoped") && (model == "fable" || model == "fable 5")
}

func claudeHasLegacyCloudCreditWindow(body []byte) bool {
	window := ojson.GetBytes(body, "iguana_necktie")
	for _, key := range []string{"limit_dollars", "used_dollars", "remaining_dollars"} {
		if value := window.Get(key); value.Exists() && value.Type != ojson.Null {
			return true
		}
	}
	return false
}

func parseZaiQuota(body []byte, _ time.Time) ([]model.ProviderQuotaWindow, error) {
	var response zaiQuotaResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode Z.ai quota response: %w", err)
	}
	if response.Code != http.StatusOK || !response.Success {
		return nil, fmt.Errorf("Z.ai quota response was unsuccessful")
	}

	windows := make([]model.ProviderQuotaWindow, 0, len(response.Data.Limits))
	for _, limit := range response.Data.Limits {
		label := fmt.Sprintf("unit%d", limit.Unit)
		switch limit.Unit {
		case 3:
			label = fmt.Sprintf("%dh", limit.Number)
		case 6:
			if limit.Number == 1 {
				label = "weekly"
			} else {
				label = fmt.Sprintf("%dw", limit.Number)
			}
		}
		window := model.ProviderQuotaWindow{
			Label:     label,
			Limit:     limit.Usage,
			Used:      limit.CurrentValue,
			Remaining: limit.Remaining,
			Percent:   limit.Percentage,
		}
		if limit.NextResetTime != 0 {
			reset := time.UnixMilli(limit.NextResetTime).UTC()
			window.ResetsAt = &reset
		}
		windows = append(windows, window)
	}
	return windows, nil
}

func parseOpenCodeGoQuota(body []byte) ([]model.ProviderQuotaWindow, error) {
	var response opencodeGoQuotaResponse
	if errUnmarshal := json.Unmarshal(body, &response); errUnmarshal != nil {
		return nil, fmt.Errorf("decode OpenCode Go quota response: %w", errUnmarshal)
	}
	rawUsage := strings.TrimSpace(string(response.Usage))
	if rawUsage == "" || rawUsage == "null" {
		return nil, nil
	}

	var usage map[string]json.RawMessage
	if errUnmarshalUsage := json.Unmarshal(response.Usage, &usage); errUnmarshalUsage != nil {
		return nil, nil
	}

	windows := make([]model.ProviderQuotaWindow, 0, len(usage))
	for _, label := range []string{"rolling", "weekly", "monthly"} {
		rawWindow, ok := usage[label]
		if !ok {
			continue
		}
		var responseWindow opencodeGoQuotaWindowResponse
		if errUnmarshalWindow := json.Unmarshal(rawWindow, &responseWindow); errUnmarshalWindow != nil {
			continue
		}
		if responseWindow.Percent == nil || *responseWindow.Percent < 0 || *responseWindow.Percent > 100 {
			continue
		}

		window := model.ProviderQuotaWindow{
			Label:     label,
			Limit:     responseWindow.Limit,
			Used:      responseWindow.Used,
			Remaining: responseWindow.Remaining,
			Percent:   responseWindow.Percent,
		}
		if responseWindow.ResetsAt != nil {
			reset, errParseReset := time.Parse(time.RFC3339, strings.TrimSpace(*responseWindow.ResetsAt))
			if errParseReset != nil {
				continue
			}
			reset = reset.UTC()
			window.ResetsAt = &reset
		}
		windows = append(windows, window)
	}
	return windows, nil
}

func providerQuotaFromWindows(windows []model.ProviderQuotaWindow) *model.ProviderQuota {
	quota := &model.ProviderQuota{Windows: windows}
	selected := -1
	for index := range windows {
		if windows[index].Label == "weekly" {
			selected = index
			break
		}
	}
	if selected < 0 && len(windows) > 0 {
		selected = 0
	}
	if selected >= 0 {
		window := windows[selected]
		quota.Limit, quota.Used, quota.Remaining, quota.ResetsAt = window.Limit, window.Used, window.Remaining, window.ResetsAt
	}
	return quota
}

// providerQuotaCooldownDecision treats every parsed window as evidence about
// the credential. It avoids changing routing when an exhausted window has no
// reliable future reset time.
func providerQuotaCooldownDecision(quota *model.ProviderQuota, now time.Time) (bool, time.Time, bool) {
	if quota == nil || len(quota.Windows) == 0 {
		return false, time.Time{}, false
	}

	exhausted := false
	resetAt := time.Time{}
	for _, window := range quota.Windows {
		if window.Percent == nil && window.Remaining == nil && (window.Limit == nil || window.Used == nil) {
			return false, time.Time{}, false
		}
		windowExhausted := false
		if window.Percent != nil && *window.Percent >= 100 {
			windowExhausted = true
		}
		if window.Remaining != nil && *window.Remaining <= 0 {
			windowExhausted = true
		}
		if window.Limit != nil && *window.Limit > 0 && window.Used != nil && *window.Used >= *window.Limit {
			windowExhausted = true
		}
		if !windowExhausted {
			continue
		}

		exhausted = true
		if window.ResetsAt == nil || !window.ResetsAt.After(now) {
			return exhausted, time.Time{}, false
		}
		if resetAt.IsZero() || window.ResetsAt.Before(resetAt) {
			resetAt = *window.ResetsAt
		}
	}
	if !exhausted {
		return false, time.Time{}, true
	}
	return exhausted, resetAt, !resetAt.IsZero()
}

func (h *Handler) applyProviderQuotaCooldownDecision(ctx context.Context, auth *coreauth.Auth, quota *model.ProviderQuota) {
	if h == nil || h.authManager == nil || auth == nil || quota == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	exhausted, resetAt, actionable := providerQuotaCooldownDecision(quota, time.Now())
	if !actionable {
		return
	}
	if _, errApply := h.authManager.ApplyProviderQuotaReport(ctx, auth.ID, exhausted, resetAt); errApply != nil {
		log.WithError(errApply).WithField("auth_id", auth.ID).Debug("failed to sync provider quota cooldown")
	}
}

func (h *Handler) syncUsageProbeCooldown(ctx context.Context, auth *coreauth.Auth, parsedURL *url.URL, body []byte) {
	if h == nil || h.authManager == nil || auth == nil || parsedURL == nil {
		return
	}
	var parse func([]byte) ([]model.ProviderQuotaWindow, error)
	switch coreauth.QuotaRecoveryProviderForAuth(auth) {
	case "zai":
		if !exactQuotaURL(parsedURL, zaiUsageQuotaURL) {
			return
		}
		parse = func(requestBody []byte) ([]model.ProviderQuotaWindow, error) {
			return parseZaiQuota(requestBody, time.Now().UTC())
		}
	case "opencode-go":
		if !exactQuotaURL(parsedURL, opencodeGoUsageQuotaURL) {
			return
		}
		parse = parseOpenCodeGoQuota
	default:
		return
	}

	windows, errParse := parse(body)
	if errParse != nil {
		log.WithError(errParse).WithField("auth_id", auth.ID).Debug("failed to parse refreshed provider usage")
		return
	}
	h.applyProviderQuotaCooldownDecision(ctx, auth, providerQuotaFromWindows(windows))
}

// syncCodexUsageCooldown reconciles each allowance independently. Code-review
// and additional-model limits must not block the whole credential.
func (h *Handler) syncCodexUsageCooldown(ctx context.Context, auth *coreauth.Auth, parsedURL *url.URL, body []byte) {
	if h == nil || h.authManager == nil || auth == nil || parsedURL == nil ||
		!strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || !exactQuotaURL(parsedURL, codexUsageQuotaURL) {
		return
	}
	if !ojson.ValidBytes(body) {
		return
	}
	root := ojson.ParseBytes(body)
	limits := firstQuotaValue(root, "additional_rate_limits", "additionalRateLimits")
	if limits.Exists() && !limits.IsArray() {
		return
	}
	additional := make(map[string]struct{})
	for _, limit := range limits.Array() {
		name := strings.TrimSpace(firstQuotaValue(limit, "limit_name", "limitName", "metered_feature", "meteredFeature").String())
		if name == "" {
			return
		}
		additional[strings.ToLower(name)] = struct{}{}
	}
	apply := func(raw json.RawMessage, models []string) {
		decision := codexRateLimitDecision(raw)
		if !decision.actionable || len(models) == 0 {
			return
		}
		var errApply error
		if !decision.exhausted {
			_, errApply = h.authManager.ApplyProviderQuotaRecovery(ctx, auth.ID, models)
		} else {
			_, errApply = h.authManager.ApplyProviderQuotaReportForModels(ctx, auth.ID, decision.exhausted, decision.resetAt, models)
		}
		if errApply != nil {
			log.WithError(errApply).WithField("auth_id", auth.ID).Debug("failed to sync Codex quota cooldown")
		}
	}
	// Exclude every independently reported family from the shared allowance.
	apply(json.RawMessage(firstQuotaValue(root, "rate_limit", "rateLimit").Raw), h.codexStandardModelIDs(auth, additional))
	for _, limit := range limits.Array() {
		name := strings.TrimSpace(firstQuotaValue(limit, "limit_name", "limitName", "metered_feature", "meteredFeature").String())
		models := h.quotaModelsForPool(auth, func(id string) bool { return strings.EqualFold(id, name) })
		apply(json.RawMessage(firstQuotaValue(limit, "rate_limit", "rateLimit").Raw), models)
	}
	apply(json.RawMessage(firstQuotaValue(root, "code_review_rate_limit", "codeReviewRateLimit").Raw), h.quotaModelsForPool(auth, func(id string) bool { return strings.EqualFold(id, "codex-auto-review") }))
}

func (h *Handler) syncClaudeUsageCooldown(ctx context.Context, auth *coreauth.Auth, parsedURL *url.URL, body []byte) {
	if h == nil || h.authManager == nil || auth == nil || parsedURL == nil ||
		!strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") || !exactQuotaURL(parsedURL, claudeUsageQuotaURL, claudeResetGrantsQuotaURL) {
		return
	}
	var usage claudeUsageResponse
	if errUnmarshal := json.Unmarshal(body, &usage); errUnmarshal != nil {
		return
	}

	baseExhausted, baseReset, baseActionable := claudeUsageBaseDecision(usage.FiveHour, usage.SevenDay, time.Now())
	resetScope, resetRead := ctx.Value(claudeResetReconcileKey{}).(claudeResetGrantScope)
	if resetRead && !baseExhausted && (usage.SevenDay == nil ||
		usage.FiveHour == nil && !resetScope.fiveHour && !ojson.GetBytes(body, "five_hour").Exists()) {
		// A healthy five-hour window alone cannot prove a weekly refund.
		// Explicit five_hour:null is the provider's windowless Ultra response.
		baseActionable = false
	}
	if baseActionable {
		var errApply error
		if baseExhausted {
			_, errApply = h.authManager.ApplyProviderQuotaReport(ctx, auth.ID, true, baseReset)
		} else {
			models := h.quotaModelsForPool(auth, func(id string) bool { return !strings.Contains(strings.ToLower(id), "fable") })
			if resetRead {
				// A partial grant's shared refund does not prove independent
				// model allowances recovered. Keep their cooldowns intact.
				models = []string{}
			}
			_, errApply = h.authManager.ApplyProviderQuotaRecovery(ctx, auth.ID, models)
		}
		if errApply != nil {
			log.WithError(errApply).WithField("auth_id", auth.ID).Debug("failed to sync Claude base quota cooldown")
		}
	}

	fable, fableFound := claudeFableUsageDecision(&usage, body)
	if !fableFound || !fable.actionable {
		return
	}
	fableModels := h.quotaModelsForPool(auth, func(id string) bool { return strings.Contains(strings.ToLower(id), "fable") })
	if len(fableModels) == 0 {
		return
	}
	if _, errApply := h.authManager.ApplyProviderQuotaReportForModels(ctx, auth.ID, fable.exhausted, fable.resetAt, fableModels); errApply != nil {
		log.WithError(errApply).WithField("auth_id", auth.ID).Debug("failed to sync Claude Fable quota cooldown")
	}
}

type claudeUsageDecision struct {
	exhausted  bool
	resetAt    time.Time
	actionable bool
}

func claudeUsageBaseDecision(fiveHour, sevenDay *claudeUsageWindowResponse, now time.Time) (bool, time.Time, bool) {
	windows := []*claudeUsageWindowResponse{fiveHour, sevenDay}
	if fiveHour == nil && sevenDay == nil {
		return false, time.Time{}, false
	}
	resetAt := time.Time{}
	for _, window := range windows {
		if window == nil {
			continue
		}
		if window.Utilization == nil || *window.Utilization < 0 {
			return false, time.Time{}, false
		}
		if *window.Utilization < 100 {
			continue
		}
		reset, errParse := time.Parse(time.RFC3339, strings.TrimSpace(window.ResetsAt))
		if errParse != nil || !reset.After(now) {
			return false, time.Time{}, false
		}
		if resetAt.IsZero() || reset.Before(resetAt) {
			resetAt = reset
		}
	}
	if !resetAt.IsZero() {
		return true, resetAt, true
	}
	return false, time.Time{}, true
}

func claudeFableUsageDecision(usage *claudeUsageResponse, body []byte) (claudeUsageDecision, bool) {
	if usage == nil {
		return claudeUsageDecision{}, false
	}
	var modern *claudeUsageLimitResponse
	for _, limit := range usage.Limits {
		if !isClaudeFableScope(limit.Kind, limit.Scope.Model.DisplayName) || limit.Percent == nil {
			continue
		}
		if limit.IsActive {
			copyLimit := limit
			modern = &copyLimit
			break
		}
		if modern == nil {
			copyLimit := limit
			modern = &copyLimit
		}
	}
	if modern != nil {
		window := &claudeUsageWindowResponse{Utilization: modern.Percent, ResetsAt: modern.ResetsAt}
		return claudeUsageWindowDecision(window, time.Now()), true
	} else if usage.IguanaNecktie != nil && !claudeHasLegacyCloudCreditWindow(body) {
		return claudeUsageWindowDecision(usage.IguanaNecktie, time.Now()), true
	}
	return claudeUsageDecision{}, false
}

func claudeUsageWindowDecision(window *claudeUsageWindowResponse, now time.Time) claudeUsageDecision {
	if window == nil {
		return claudeUsageDecision{}
	}
	if window.Utilization == nil {
		return claudeUsageDecision{}
	}
	if *window.Utilization < 100 {
		return claudeUsageDecision{actionable: true}
	}
	reset, errParse := time.Parse(time.RFC3339, strings.TrimSpace(window.ResetsAt))
	if errParse != nil || !reset.After(now) {
		return claudeUsageDecision{}
	}
	return claudeUsageDecision{exhausted: true, resetAt: reset, actionable: true}
}

func (h *Handler) codexStandardModelIDs(auth *coreauth.Auth, additional map[string]struct{}) []string {
	return h.quotaModelsForPool(auth, func(id string) bool {
		key := strings.ToLower(id)
		_, reported := additional[key]
		return key != "codex-auto-review" && !strings.HasSuffix(key, "-spark") && !reported
	})
}

func codexRateLimitFamilyHealthy(raw json.RawMessage) bool {
	var rateLimit codexRateLimitResponse
	if errUnmarshal := json.Unmarshal(raw, &rateLimit); errUnmarshal != nil {
		return false
	}
	limitReached := rateLimit.LimitReached
	if limitReached == nil {
		limitReached = rateLimit.LimitReachedCamel
	}
	if rateLimit.Allowed != nil && !*rateLimit.Allowed || limitReached != nil && *limitReached {
		return false
	}
	windows := []json.RawMessage{rateLimit.PrimaryWindow, rateLimit.PrimaryWindowCamel, rateLimit.SecondaryWindow, rateLimit.SecondaryWindowCamel}
	observed := false
	for _, rawWindow := range windows {
		if len(rawWindow) == 0 || string(rawWindow) == "null" {
			continue
		}
		var window codexUsageWindowResponse
		if errUnmarshal := json.Unmarshal(rawWindow, &window); errUnmarshal != nil {
			return false
		}
		usedPercentRaw := window.UsedPercent
		if len(usedPercentRaw) == 0 || string(usedPercentRaw) == "null" {
			usedPercentRaw = window.UsedPercentCamel
		}
		if len(usedPercentRaw) == 0 || string(usedPercentRaw) == "null" {
			return false
		}
		var usedPercent float64
		if errUnmarshal := json.Unmarshal(usedPercentRaw, &usedPercent); errUnmarshal != nil {
			var value string
			if errUnmarshal = json.Unmarshal(usedPercentRaw, &value); errUnmarshal != nil {
				return false
			}
			parsed, errParse := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if errParse != nil {
				return false
			}
			usedPercent = parsed
		}
		if math.IsNaN(usedPercent) || math.IsInf(usedPercent, 0) || usedPercent < 0 || usedPercent >= 100 {
			return false
		}
		observed = true
	}
	return observed
}

func runUsageProbe(ctx context.Context, h *Handler, auth *coreauth.Auth, name, targetURL string, parse func([]byte) ([]model.ProviderQuotaWindow, error)) (*model.ProviderQuota, error) {
	windows, errProbe := runUsageProbeWindows(ctx, h, auth, name, targetURL, parse)
	if errProbe != nil {
		return nil, errProbe
	}
	return providerQuotaFromWindows(windows), nil
}

func runUsageProbeWindows(ctx context.Context, h *Handler, auth *coreauth.Auth, name, targetURL string, parse func([]byte) ([]model.ProviderQuotaWindow, error)) ([]model.ProviderQuotaWindow, error) {
	return runUsageProbeWindowsWithMode(ctx, h, auth, name, targetURL, parse, false)
}

func runUsageProbeWindowsWithMode(ctx context.Context, h *Handler, auth *coreauth.Auth, name, targetURL string, parse func([]byte) ([]model.ProviderQuotaWindow, error), background bool) ([]model.ProviderQuotaWindow, error) {
	outcome, errProbe := runUsageProbeResponse(ctx, h, auth, name, targetURL, background)
	if errProbe != nil {
		return nil, errProbe
	}
	return parse([]byte(outcome.response.Body))
}

func runUsageProbeResponse(ctx context.Context, h *Handler, auth *coreauth.Auth, name, targetURL string, background bool) (quotaCallOutcome, error) {
	parsedURL, errParseURL := url.Parse(targetURL)
	if errParseURL != nil {
		return quotaCallOutcome{}, fmt.Errorf("parse %s quota URL: %w", name, errParseURL)
	}
	headers := map[string]string{
		"Accept":        "application/json",
		"Authorization": "Bearer $TOKEN$",
	}
	switch coreauth.QuotaRecoveryProviderForAuth(auth) {
	case "claude":
		headers["Anthropic-Beta"] = "oauth-2025-04-20"
		headers["User-Agent"] = "claude-cli/2.1.280 (external, cli)"
		if h.cfg != nil && strings.TrimSpace(h.cfg.ClaudeHeaderDefaults.UserAgent) != "" {
			headers["User-Agent"] = strings.TrimSpace(h.cfg.ClaudeHeaderDefaults.UserAgent)
		}
	case "codex":
		if accountID, ok := auth.Metadata["account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
			headers["Chatgpt-Account-Id"] = strings.TrimSpace(accountID)
		}
		if h.cfg != nil && strings.TrimSpace(h.cfg.CodexHeaderDefaults.UserAgent) != "" {
			headers["User-Agent"] = strings.TrimSpace(h.cfg.CodexHeaderDefaults.UserAgent)
		}
	}
	key := buildQuotaCacheKey(h, auth, http.MethodGet, parsedURL, "", headers, "")
	execute := func(requestContext context.Context) quotaCallOutcome {
		return h.executeAPICall(requestContext, http.MethodGet, targetURL, "", auth, "", headers, "")
	}
	var outcome quotaCallOutcome
	if background {
		outcome = h.getQuotaCache().doBackground(ctx, key, execute)
	} else {
		outcome = h.getQuotaCache().do(ctx, key, execute)
	}
	if outcome.outerError != "" {
		return quotaCallOutcome{}, fmt.Errorf("%s usage request failed: %s", name, outcome.outerError)
	}
	if !outcome.successfulResponse() {
		return quotaCallOutcome{}, fmt.Errorf("%s usage request returned status %d", name, outcome.response.StatusCode)
	}
	return outcome, nil
}

func parseClaudeGSRQuota(body []byte) ([]model.ProviderQuotaWindow, error) {
	var usage claudeUsageResponse
	if errUnmarshal := json.Unmarshal(body, &usage); errUnmarshal != nil {
		return nil, fmt.Errorf("decode Claude usage response: %w", errUnmarshal)
	}
	windows := make([]model.ProviderQuotaWindow, 0, 2)
	if usage.SevenDay != nil {
		if reset, errParse := time.Parse(time.RFC3339, strings.TrimSpace(usage.SevenDay.ResetsAt)); errParse == nil && !reset.IsZero() {
			windows = append(windows, model.ProviderQuotaWindow{Label: "weekly", ResetsAt: &reset})
		}
	}
	var fableReset string
	for _, limit := range usage.Limits {
		if isClaudeFableScope(limit.Kind, limit.Scope.Model.DisplayName) && strings.TrimSpace(limit.ResetsAt) != "" {
			if limit.IsActive || fableReset == "" {
				fableReset = strings.TrimSpace(limit.ResetsAt)
			}
		}
	}
	if fableReset == "" && usage.IguanaNecktie != nil && !claudeHasLegacyCloudCreditWindow(body) {
		fableReset = strings.TrimSpace(usage.IguanaNecktie.ResetsAt)
	}
	if reset, errParse := time.Parse(time.RFC3339, fableReset); errParse == nil && !reset.IsZero() {
		windows = append(windows, model.ProviderQuotaWindow{Label: "weekly_fable", ResetsAt: &reset})
	}
	return windows, nil
}

func parseCodexGSRQuota(body []byte) ([]model.ProviderQuotaWindow, error) {
	return parseCodexGSRQuotaAt(body, time.Now().UTC())
}

func parseCodexGSRQuotaAt(body []byte, observedAt time.Time) ([]model.ProviderQuotaWindow, error) {
	var usage map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &usage); errUnmarshal != nil {
		return nil, fmt.Errorf("decode Codex usage response: %w", errUnmarshal)
	}
	raw := usage["rate_limit"]
	if len(raw) == 0 || string(raw) == "null" {
		raw = usage["rateLimit"]
	}
	var rateLimit codexRateLimitResponse
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &rateLimit) != nil {
		return nil, fmt.Errorf("Codex usage response has no rate limit windows")
	}
	var bestReset time.Time
	var bestSeconds float64
	now := time.Now()
	for _, raw := range []json.RawMessage{
		rateLimit.PrimaryWindow, rateLimit.PrimaryWindowCamel, rateLimit.SecondaryWindow, rateLimit.SecondaryWindowCamel,
	} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var window codexUsageWindowResponse
		if json.Unmarshal(raw, &window) != nil {
			continue
		}
		reset := parseCodexUsageWindowResetAt(window, observedAt)
		if !reset.After(now) {
			continue
		}
		seconds := max(rawFloat(window.WindowSeconds), rawFloat(window.WindowSecondsCam))
		if seconds == 0 {
			seconds = 60 * max(rawFloat(window.WindowMinutes), rawFloat(window.WindowMinutesCam))
		}
		if bestReset.IsZero() || seconds > bestSeconds {
			bestReset = reset
			bestSeconds = seconds
		}
	}
	if bestReset.IsZero() {
		return nil, fmt.Errorf("Codex usage response has no usable weekly reset")
	}
	return []model.ProviderQuotaWindow{{Label: "weekly", ResetsAt: &bestReset}}, nil
}

func rawFloat(raw json.RawMessage) float64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var value float64
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var textValue string
	if json.Unmarshal(raw, &textValue) == nil {
		value, _ = strconv.ParseFloat(strings.TrimSpace(textValue), 64)
	}
	return value
}

func parseCodexUsageWindowReset(window codexUsageWindowResponse) time.Time {
	return parseCodexUsageWindowResetAt(window, time.Now().UTC())
}

func parseCodexUsageWindowResetAt(window codexUsageWindowResponse, observedAt time.Time) time.Time {
	for _, raw := range []json.RawMessage{window.ResetAt, window.ResetAtCamel} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		if value := rawFloat(raw); value > 0 {
			return time.Unix(int64(value), 0).UTC()
		}
		var textValue string
		if json.Unmarshal(raw, &textValue) == nil {
			if parsed, errParse := time.Parse(time.RFC3339, strings.TrimSpace(textValue)); errParse == nil {
				return parsed.UTC()
			}
		}
	}
	for _, raw := range []json.RawMessage{window.ResetAfter, window.ResetAfterCamel} {
		if len(raw) > 0 && string(raw) != "null" {
			if value := rawFloat(raw); value >= 0 {
				return observedAt.Add(time.Duration(value * float64(time.Second)))
			}
		}
	}
	return time.Time{}
}

func kiroAuthValue(auth *coreauth.Auth, keys ...string) string {
	if auth == nil {
		return ""
	}
	for _, key := range keys {
		if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
			return value
		}
	}
	for _, key := range keys {
		if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func kiroUsageLimitsURL(auth *coreauth.Auth) string {
	region := kiroAuthValue(auth, "region")
	if region == "" {
		region = kiroauth.DefaultRegion
	}
	endpoint := strings.TrimRight(kiroUsageLimitsEndpoint(region), "/") + "/getUsageLimits"
	parsedURL, errParseURL := url.Parse(endpoint)
	if errParseURL != nil {
		return ""
	}
	query := parsedURL.Query()
	query.Set("origin", "AI_EDITOR")
	query.Set("resourceType", "AGENTIC_REQUEST")
	query.Set("isEmailRequired", "true")
	if profileARN := kiroAuthValue(auth, "profile_arn", "profileArn"); profileARN != "" {
		query.Set("profileArn", profileARN)
	}
	parsedURL.RawQuery = query.Encode()
	return parsedURL.String()
}

// firstGJSON returns the first present field, mirroring the CPAMC Kiro
// adapter's camelCase/PascalCase and precision-field fallbacks.
func firstGJSON(value gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		if field := value.Get(path); field.Exists() && field.Type != gjson.Null {
			return field
		}
	}
	return gjson.Result{}
}

// parseKiroGSRQuota returns the included monthly request window and the
// subscription title used by the GSR routing probe. Free-trial windows do not
// describe the recurring allowance, so only the breakdown reset is used.
func parseKiroGSRQuota(body []byte) ([]model.ProviderQuotaWindow, string, error) {
	if !gjson.ValidBytes(body) {
		return nil, "", fmt.Errorf("decode Kiro usage response: invalid JSON")
	}
	root := gjson.ParseBytes(body)
	plan := strings.TrimSpace(firstGJSON(firstGJSON(root, "subscriptionInfo", "SubscriptionInfo"), "subscriptionTitle", "subscription_title", "title").String())
	for _, breakdown := range firstGJSON(root, "usageBreakdownList", "UsageBreakdownList").Array() {
		resetSeconds := firstGJSON(breakdown, "nextDateReset", "nextDateResetUnix").Float()
		if resetSeconds <= 0 {
			continue
		}
		resetAt := time.Unix(0, int64(resetSeconds*float64(time.Second))).UTC()
		window := model.ProviderQuotaWindow{Label: "monthly", ResetsAt: &resetAt}
		limitField := firstGJSON(breakdown, "usageLimitWithPrecision", "usageLimit")
		usedField := firstGJSON(breakdown, "currentUsageWithPrecision", "currentUsage")
		total, usage := limitField.Float(), usedField.Float()
		if limitField.Exists() && usedField.Exists() && total > 0 && !math.IsInf(total, 1) && usage >= 0 && !math.IsInf(usage, 1) {
			limit, used := int64(math.Round(total)), int64(math.Round(usage))
			remaining := max(limit-used, 0)
			percent := math.Min(100, usage/total*100)
			window.Limit, window.Used, window.Remaining, window.Percent = &limit, &used, &remaining, &percent
		}
		return []model.ProviderQuotaWindow{window}, plan, nil
	}
	return nil, plan, fmt.Errorf("Kiro usage response has no usable monthly reset")
}

// RefreshQuotaForRouting synchronously refreshes and persists the provider
// window used by greedy-soonest-recovery. It shares the management quota cache
// with CPAMC requests, so a recent management read avoids another provider call.
func (h *Handler) RefreshQuotaForRouting(ctx context.Context, auth *coreauth.Auth, modelName string) error {
	if h == nil || h.authManager == nil || auth == nil {
		return fmt.Errorf("quota routing refresh is unavailable")
	}
	// Resolve the credential's quota identity from its configured usage probe
	// first: a credential served through a different adapter must still be
	// probed at, and parsed by, the provider that owns its quota window.
	provider := coreauth.QuotaRecoveryProviderForAuth(auth)
	ctx = h.authManager.CaptureQuotaObservationForAuth(ctx, auth)
	var name, targetURL string
	switch provider {
	case "claude":
		name, targetURL = "Claude", claudeUsageQuotaURL
	case "codex":
		name, targetURL = "Codex", codexUsageQuotaURL
	case "opencode-go":
		name, targetURL = "OpenCode Go", opencodeGoUsageQuotaURL
	case "zai":
		name, targetURL = "Z.ai", zaiUsageQuotaURL
	case "kiro":
		name, targetURL = "Kiro", kiroUsageLimitsURL(auth)
	default:
		if probe := strings.ToLower(strings.TrimSpace(authAttribute(auth, "usage_probe"))); probe != "" {
			return fmt.Errorf("usage probe %q has no quota recovery probe", probe)
		}
		return fmt.Errorf("provider %q has no quota recovery probe", strings.ToLower(strings.TrimSpace(auth.Provider)))
	}
	outcome, errProbe := runUsageProbeResponse(ctx, h, auth, name, targetURL, false)
	if errProbe != nil {
		return errProbe
	}
	observedAt := outcome.cachedAt
	body := []byte(outcome.response.Body)
	var (
		windows []model.ProviderQuotaWindow
		bars    []coreauth.QuotaReserveBar
		plan    string
		err     error
	)
	switch provider {
	case "claude":
		windows, err = parseClaudeGSRQuota(body)
		bars, _ = claudeQuotaReserveBars(body)
	case "codex":
		windows, err = parseCodexGSRQuotaAt(body, observedAt)
		bars, _ = codexQuotaReserveBarsAt(body, observedAt)
		plan = ojson.GetBytes(body, "plan_type").String()
	case "opencode-go":
		bars, err = opencodeGoQuotaReserveBars(body)
		for _, bar := range bars {
			windows = append(windows, model.ProviderQuotaWindow{
				Label: strings.TrimPrefix(bar.ID, "opencode-go-"), ResetsAt: new(bar.ResetAt),
			})
		}
	case "zai":
		plan = ojson.GetBytes(body, "data.level").String()
		windows, err = parseZaiQuota(body, observedAt)
	case "kiro":
		windows, plan, err = parseKiroGSRQuota(body)
	}
	if err != nil {
		return err
	}
	if provider == "zai" || provider == "kiro" {
		for _, window := range windows {
			if window.ResetsAt == nil {
				continue
			}
			var remaining float64
			switch {
			case provider == "kiro" && window.Percent != nil:
				remaining = 100 - *window.Percent
			case window.Limit != nil && *window.Limit <= 0:
				continue
			case window.Limit != nil && *window.Limit > 0 && window.Remaining != nil:
				remaining = 100 * float64(*window.Remaining) / float64(*window.Limit)
			case window.Percent != nil:
				remaining = 100 - *window.Percent
			default:
				continue
			}
			bars = append(bars, coreauth.QuotaReserveBar{ID: window.Label, RemainingPercent: remaining, ResetAt: *window.ResetsAt})
		}
	}
	modelKey := strings.ToLower(strings.TrimSpace(modelName))
	preferredLabels := []string{"weekly", "monthly"}
	signalName := "weekly_reset_at"
	switch {
	case provider == "claude" && strings.Contains(modelKey, "fable"):
		preferredLabels = []string{"weekly_fable", "weekly"}
		signalName = "Anthropic-Ratelimit-Unified-7d_oi-Reset"
	case provider == "claude":
		signalName = "Anthropic-Ratelimit-Unified-7d-Reset"
	case provider == "codex":
		signalName = "X-Codex-Primary-Reset-At"
	case provider == "opencode-go":
		preferredLabels = []string{"monthly"}
		signalName = "monthly_reset_at"
	case provider == "kiro":
		preferredLabels = []string{"monthly"}
		signalName = "monthly_reset_at"
	}
	var resetAt time.Time
	for _, label := range preferredLabels {
		for _, window := range windows {
			if strings.EqualFold(window.Label, label) && window.ResetsAt != nil && window.ResetsAt.After(time.Now()) {
				resetAt = *window.ResetsAt
				break
			}
		}
		if !resetAt.IsZero() {
			break
		}
	}
	if resetAt.IsZero() {
		return fmt.Errorf("provider %q returned no usable recovery timestamp", provider)
	}
	manual := auth.Quota.ManualReset
	if provider == "codex" || provider == "claude" {
		observation := coreauth.ManualResetObservation{Provider: provider}
		targetURL := codexResetCreditsURL
		if provider == "claude" {
			targetURL = claudeResetGrantsQuotaURL
		}
		manualOutcome, errManual := runUsageProbeResponse(ctx, h, auth, provider, targetURL, false)
		if errManual == nil {
			observation.ExpiresAt, errManual = parseManualResetExpiry(provider, []byte(manualOutcome.response.Body), time.Now().UTC())
		}
		if errManual != nil {
			log.WithError(errManual).WithField("auth_id", auth.ID).Debug("manual reset expiry unavailable; preserving previous observation")
		} else {
			observation.ObservedAt = manualOutcome.cachedAt
			manual = observation
		}
	}
	// A probe is a complete snapshot. Copying passive headers here can attach
	// their usage to the wrong window or re-anchor a relative reset.
	quota := coreauth.QuotaState{ObservedAt: observedAt, ManualReset: manual, Signals: make(map[string]string)}
	if provider == "claude" {
		for _, window := range windows {
			if window.ResetsAt == nil || !window.ResetsAt.After(time.Now()) {
				continue
			}
			if window.Label == "weekly" {
				quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = window.ResetsAt.UTC().Format(time.RFC3339)
			} else if window.Label == "weekly_fable" {
				quota.Signals["Anthropic-Ratelimit-Unified-7d_oi-Reset"] = window.ResetsAt.UTC().Format(time.RFC3339)
			}
		}
	} else {
		quota.Signals[signalName] = resetAt.UTC().Format(time.RFC3339)
	}
	quota.SetGSRQuotaWindows(provider, bars)
	if plan != "" {
		quota.Signals[provider+"_plan_type"] = plan
	}
	_, errUpdate := h.authManager.UpdateQuotaObservation(ctx, auth.ID, quota)
	return errUpdate
}

func probeZaiUsage(ctx context.Context, h *Handler, auth *coreauth.Auth) (*model.ProviderQuota, error) {
	return runUsageProbe(ctx, h, auth, "Z.ai", zaiUsageQuotaURL, func(body []byte) ([]model.ProviderQuotaWindow, error) {
		return parseZaiQuota(body, time.Now().UTC())
	})
}

func probeOpenCodeGoUsage(ctx context.Context, h *Handler, auth *coreauth.Auth) (*model.ProviderQuota, error) {
	return runUsageProbe(ctx, h, auth, "OpenCode Go", opencodeGoUsageQuotaURL, parseOpenCodeGoQuota)
}

var usageProbes = map[string]func(context.Context, *Handler, *coreauth.Auth) (*model.ProviderQuota, error){
	"zai":         probeZaiUsage,
	"opencode-go": probeOpenCodeGoUsage,
}

func probeUsage(ctx context.Context, h *Handler, auth *coreauth.Auth) (*model.ProviderQuota, error) {
	probeName := strings.ToLower(strings.TrimSpace(authAttribute(auth, "usage_probe")))
	probe, ok := usageProbes[probeName]
	if !ok || probe == nil {
		return nil, fmt.Errorf("unsupported usage probe %q", probeName)
	}
	return probe(ctx, h, auth)
}

type usageProbeResult struct {
	key   string
	quota *model.ProviderQuota
	err   error
}

func (h *Handler) applyUsageProbes(ctx context.Context, credentials map[string]*coreauth.Auth, rows map[string]model.ProviderCredential) {
	if len(credentials) == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	results := make(chan usageProbeResult, len(credentials))
	semaphore := make(chan struct{}, usageProbeMaxConcurrency)
	var waitGroup sync.WaitGroup
	for key, auth := range credentials {
		waitGroup.Add(1)
		go func(key string, auth *coreauth.Auth) {
			defer waitGroup.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-semaphore }()
			quota, errProbe := probeUsage(ctx, h, auth)
			results <- usageProbeResult{key: key, quota: quota, err: errProbe}
		}(key, auth)
	}
	waitGroup.Wait()
	close(results)
	for result := range results {
		if result.err != nil {
			log.WithError(result.err).Debug("management usage probe failed")
			continue
		}
		row, ok := rows[result.key]
		if ok {
			row.Quota = result.quota
			rows[result.key] = row
		}
	}
}
