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

	kiroauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
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
			Type          string  `json:"type"`
			Unit          int     `json:"unit"`
			Number        int     `json:"number"`
			Usage         int64   `json:"usage"`
			CurrentValue  int64   `json:"currentValue"`
			Remaining     int64   `json:"remaining"`
			Percentage    float64 `json:"percentage"`
			NextResetTime int64   `json:"nextResetTime"`
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
		used, remaining, percent := limit.CurrentValue, limit.Remaining, limit.Percentage
		window := model.ProviderQuotaWindow{
			Label:     label,
			Limit:     &limit.Usage,
			Used:      &used,
			Remaining: &remaining,
			Percent:   &percent,
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
	switch strings.ToLower(strings.TrimSpace(authAttribute(auth, "usage_probe"))) {
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

// syncCodexUsageCooldown only clears stale Codex cooldowns when the standard
// request allowance reports healthy. Code-review and additional-model limits
// are independent and must not block the whole credential.
func (h *Handler) syncCodexUsageCooldown(ctx context.Context, auth *coreauth.Auth, parsedURL *url.URL, body []byte) {
	if h == nil || h.authManager == nil || auth == nil || parsedURL == nil ||
		!strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || !exactQuotaURL(parsedURL, codexUsageQuotaURL) {
		return
	}
	if !codexStandardAllowanceHealthy(body) {
		return
	}
	additional, ok := codexAdditionalModelIDs(body)
	if !ok {
		return
	}
	if _, errApply := h.authManager.ApplyProviderQuotaReportForModels(ctx, auth.ID, false, time.Time{}, codexStandardModelIDs(auth.ID, additional)); errApply != nil {
		log.WithError(errApply).WithField("auth_id", auth.ID).Debug("failed to clear Codex cooldown from healthy usage report")
	}
}

func (h *Handler) syncClaudeUsageCooldown(ctx context.Context, auth *coreauth.Auth, parsedURL *url.URL, body []byte) {
	if h == nil || h.authManager == nil || auth == nil || parsedURL == nil ||
		!strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") || !exactQuotaURL(parsedURL, claudeUsageQuotaURL) {
		return
	}
	var usage claudeUsageResponse
	if errUnmarshal := json.Unmarshal(body, &usage); errUnmarshal != nil {
		return
	}

	baseExhausted, baseReset, baseActionable := claudeUsageBaseDecision(usage.FiveHour, usage.SevenDay, time.Now())
	if baseActionable {
		if _, errApply := h.authManager.ApplyProviderQuotaReport(ctx, auth.ID, baseExhausted, baseReset); errApply != nil {
			log.WithError(errApply).WithField("auth_id", auth.ID).Debug("failed to sync Claude base quota cooldown")
		}
	}

	fable, fableFound := claudeFableUsageDecision(&usage)
	if !fableFound || !fable.actionable {
		return
	}
	fableModels := claudeUsageModelIDs(auth.ID, "fable")
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
		if window == nil || window.Utilization == nil || *window.Utilization < 100 {
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

func claudeFableUsageDecision(usage *claudeUsageResponse) (claudeUsageDecision, bool) {
	if usage == nil {
		return claudeUsageDecision{}, false
	}
	var modern *claudeUsageLimitResponse
	for _, limit := range usage.Limits {
		if !strings.EqualFold(strings.TrimSpace(limit.Kind), "weekly_scoped") ||
			!strings.EqualFold(strings.TrimSpace(limit.Scope.Model.DisplayName), "fable") || limit.Percent == nil {
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
	} else if usage.IguanaNecktie != nil {
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

func claudeUsageModelIDs(authID, family string) []string {
	family = strings.ToLower(strings.TrimSpace(family))
	if family == "" {
		return nil
	}
	models := registry.GetGlobalRegistry().GetModelsForClient(authID)
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if model == nil || !strings.Contains(strings.ToLower(model.ID), family) {
			continue
		}
		ids = append(ids, model.ID)
	}
	return ids
}

func codexStandardModelIDs(authID string, additional map[string]bool) []string {
	models := registry.GetGlobalRegistry().GetModelsForClient(authID)
	standard := make([]string, 0, len(models))
	for _, model := range models {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(model.ID))
		if key == "codex-auto-review" {
			continue
		}
		if strings.HasSuffix(key, "-spark") {
			healthy, reported := additional[key]
			if !reported || !healthy {
				continue
			}
		}
		if healthy, reported := additional[key]; reported && !healthy {
			continue
		}
		standard = append(standard, model.ID)
	}
	return standard
}

func codexAdditionalModelIDs(body []byte) (map[string]bool, bool) {
	var usage map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &usage); errUnmarshal != nil {
		return nil, false
	}
	raw := usage["additional_rate_limits"]
	if len(raw) == 0 || string(raw) == "null" {
		raw = usage["additionalRateLimits"]
	}
	additional := make(map[string]bool)
	if len(raw) == 0 || string(raw) == "null" {
		return additional, true
	}
	var limits []struct {
		LimitName           string          `json:"limit_name"`
		LimitNameCamel      string          `json:"limitName"`
		MeteredFeature      string          `json:"metered_feature"`
		MeteredFeatureCamel string          `json:"meteredFeature"`
		RateLimit           json.RawMessage `json:"rate_limit"`
		RateLimitCamel      json.RawMessage `json:"rateLimit"`
	}
	if errUnmarshal := json.Unmarshal(raw, &limits); errUnmarshal != nil {
		return nil, false
	}
	for _, limit := range limits {
		name := strings.TrimSpace(limit.LimitName)
		if name == "" {
			name = strings.TrimSpace(limit.LimitNameCamel)
		}
		if name == "" {
			name = strings.TrimSpace(limit.MeteredFeature)
		}
		if name == "" {
			name = strings.TrimSpace(limit.MeteredFeatureCamel)
		}
		if name == "" {
			return nil, false
		}
		rateLimit := limit.RateLimit
		if len(rateLimit) == 0 || string(rateLimit) == "null" {
			rateLimit = limit.RateLimitCamel
		}
		additional[strings.ToLower(name)] = len(rateLimit) > 0 && string(rateLimit) != "null" && codexRateLimitFamilyHealthy(rateLimit)
	}
	return additional, true
}

func codexStandardAllowanceHealthy(body []byte) bool {
	var usage map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &usage); errUnmarshal != nil {
		return false
	}
	standard := usage["rate_limit"]
	if len(standard) == 0 || string(standard) == "null" {
		standard = usage["rateLimit"]
	}
	return len(standard) > 0 && string(standard) != "null" && codexRateLimitFamilyHealthy(standard)
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
	parsedURL, errParseURL := url.Parse(targetURL)
	if errParseURL != nil {
		return nil, fmt.Errorf("parse %s quota URL: %w", name, errParseURL)
	}
	headers := map[string]string{
		"Accept":        "application/json",
		"Authorization": "Bearer $TOKEN$",
	}
	key := buildQuotaCacheKey(h, auth, http.MethodGet, parsedURL, "", headers, "")
	outcome := h.getQuotaCache().do(ctx, key, func(requestContext context.Context) quotaCallOutcome {
		return h.executeAPICall(requestContext, http.MethodGet, targetURL, "", auth, "", headers, "")
	})
	if outcome.outerError != "" {
		return nil, fmt.Errorf("%s usage request failed: %s", name, outcome.outerError)
	}
	if !outcome.successfulResponse() {
		return nil, fmt.Errorf("%s usage request returned status %d", name, outcome.response.StatusCode)
	}
	windows, errParseQuota := parse([]byte(outcome.response.Body))
	if errParseQuota != nil {
		return nil, errParseQuota
	}
	return windows, nil
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
		if strings.EqualFold(strings.TrimSpace(limit.Kind), "weekly_scoped") && strings.EqualFold(strings.TrimSpace(limit.Scope.Model.DisplayName), "fable") && strings.TrimSpace(limit.ResetsAt) != "" {
			if limit.IsActive || fableReset == "" {
				fableReset = strings.TrimSpace(limit.ResetsAt)
			}
		}
	}
	if fableReset == "" && usage.IguanaNecktie != nil {
		fableReset = strings.TrimSpace(usage.IguanaNecktie.ResetsAt)
	}
	if reset, errParse := time.Parse(time.RFC3339, fableReset); errParse == nil && !reset.IsZero() {
		windows = append(windows, model.ProviderQuotaWindow{Label: "weekly_fable", ResetsAt: &reset})
	}
	return windows, nil
}

func parseCodexGSRQuota(body []byte) ([]model.ProviderQuotaWindow, error) {
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
	var bestMinutes float64
	var bestRank = 99
	for rank, item := range []struct {
		label string
		raw   json.RawMessage
	}{
		{label: "weekly", raw: rateLimit.PrimaryWindow},
		{label: "weekly", raw: rateLimit.PrimaryWindowCamel},
		{label: "weekly", raw: rateLimit.SecondaryWindow},
		{label: "weekly", raw: rateLimit.SecondaryWindowCamel},
	} {
		if len(item.raw) == 0 || string(item.raw) == "null" {
			continue
		}
		var window codexUsageWindowResponse
		if json.Unmarshal(item.raw, &window) != nil {
			continue
		}
		reset := parseCodexUsageWindowReset(window)
		if reset.IsZero() || !reset.After(time.Now()) {
			continue
		}
		minutes := rawFloat(window.WindowMinutes)
		if minutes == 0 {
			minutes = rawFloat(window.WindowMinutesCam)
		}
		if bestReset.IsZero() || minutes > bestMinutes || (minutes == bestMinutes && rank < bestRank) {
			bestReset = reset
			bestMinutes = minutes
			bestRank = rank
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
				return time.Now().UTC().Add(time.Duration(value * float64(time.Second)))
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
		if limitField := firstGJSON(breakdown, "usageLimitWithPrecision", "usageLimit"); limitField.Exists() {
			limit := int64(math.Max(0, math.Round(limitField.Float())))
			used := int64(math.Max(0, math.Round(firstGJSON(breakdown, "currentUsageWithPrecision", "currentUsage").Float())))
			remaining := max(limit-used, 0)
			percent := 0.0
			if limit > 0 {
				percent = math.Min(100, float64(used)/float64(limit)*100)
			}
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
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	var (
		windows []model.ProviderQuotaWindow
		plan    string
		err     error
	)
	switch {
	case provider == "claude":
		windows, err = runUsageProbeWindows(ctx, h, auth, "Claude", claudeUsageQuotaURL, parseClaudeGSRQuota)
	case provider == "codex":
		windows, err = runUsageProbeWindows(ctx, h, auth, "Codex", codexUsageQuotaURL, parseCodexGSRQuota)
	case strings.Contains(provider, "opencode") && strings.Contains(provider, "go"):
		windows, err = runUsageProbeWindows(ctx, h, auth, "OpenCode Go", opencodeGoUsageQuotaURL, parseOpenCodeGoQuota)
	case provider == "zai" || strings.Contains(provider, "zai"):
		windows, err = runUsageProbeWindows(ctx, h, auth, "Z.ai", zaiUsageQuotaURL, func(body []byte) ([]model.ProviderQuotaWindow, error) {
			return parseZaiQuota(body, time.Now().UTC())
		})
	case provider == "kiro":
		windows, err = runUsageProbeWindows(ctx, h, auth, "Kiro", kiroUsageLimitsURL(auth), func(body []byte) ([]model.ProviderQuotaWindow, error) {
			parsedWindows, parsedPlan, errParseQuota := parseKiroGSRQuota(body)
			plan = parsedPlan
			return parsedWindows, errParseQuota
		})
	default:
		return fmt.Errorf("provider %q has no quota recovery probe", provider)
	}
	if err != nil {
		return err
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
	case strings.Contains(provider, "opencode"):
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
	quota := auth.Quota.Clone()
	if quota.Signals == nil {
		quota.Signals = make(map[string]string)
	}
	quota.Signals[signalName] = resetAt.UTC().Format(time.RFC3339)
	if provider == "kiro" && plan != "" {
		quota.Signals["kiro_plan_type"] = plan
	}
	quota.ObservedAt = time.Now().UTC()
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
		if credential := credentials[result.key]; credential != nil {
			h.applyProviderQuotaCooldownDecision(ctx, credential, result.quota)
		}
		row, ok := rows[result.key]
		if ok {
			row.Quota = result.quota
			rows[result.key] = row
		}
	}
}
