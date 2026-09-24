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

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const zaiUsageQuotaURL = "https://api.z.ai/api/monitor/usage/quota/limit"

const usageProbeMaxConcurrency = 8

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
		return h.executeAPICall(requestContext, http.MethodGet, targetURL, auth, "", headers, "")
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
	return providerQuotaFromWindows(windows), nil
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
