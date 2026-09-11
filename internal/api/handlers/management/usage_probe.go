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

func probeUsage(ctx context.Context, h *Handler, auth *coreauth.Auth) (*model.ProviderQuota, error) {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(authAttribute(auth, "usage_probe")), "zai") {
		return nil, fmt.Errorf("unsupported usage probe")
	}
	parsedURL, errParseURL := url.Parse(zaiUsageQuotaURL)
	if errParseURL != nil {
		return nil, fmt.Errorf("parse Z.ai quota URL: %w", errParseURL)
	}
	headers := map[string]string{
		"Accept":        "application/json",
		"Authorization": "Bearer $TOKEN$",
	}
	key := buildQuotaCacheKey(h, auth, http.MethodGet, parsedURL, "", headers, "")
	outcome := h.getQuotaCache().do(ctx, key, func(requestContext context.Context) quotaCallOutcome {
		return h.executeAPICall(requestContext, http.MethodGet, zaiUsageQuotaURL, auth, "", headers, "")
	})
	if outcome.outerError != "" {
		return nil, fmt.Errorf("Z.ai usage request failed: %s", outcome.outerError)
	}
	if !outcome.successfulResponse() {
		return nil, fmt.Errorf("Z.ai usage request returned status %d", outcome.response.StatusCode)
	}
	windows, errParseQuota := parseZaiQuota([]byte(outcome.response.Body), time.Now().UTC())
	if errParseQuota != nil {
		return nil, errParseQuota
	}
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
	return quota, nil
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
