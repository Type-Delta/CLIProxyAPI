package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
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
		row, ok := rows[result.key]
		if ok {
			row.Quota = result.quota
			rows[result.key] = row
		}
	}
}
