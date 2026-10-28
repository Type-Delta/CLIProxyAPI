package management

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type quotaHistoryService interface {
	RecordQuotaHistory(context.Context, []store.QuotaHistorySample) error
	QuotaHistory(context.Context, string, time.Time, time.Time) ([]store.QuotaHistorySample, error)
	CredentialID(string, string, string) (*string, error)
}

func validQuotaHistoryProvider(provider string) bool {
	switch provider {
	case "antigravity", "claude", "codex", "devin", "kimi", "kiro", "opencode-go", "meta", "plugin", "xai", "zai":
		return true
	}
	return false
}

var quotaHistoryIdentifier = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,64}$`)

func validQuotaHistoryNumber(value *float64, maxValue float64) bool {
	return value == nil || !math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= 0 && *value <= maxValue
}

func (h *Handler) quotaHistoryService() (quotaHistoryService, error) {
	service, err := h.analyticsServiceForRead()
	if err != nil {
		return nil, err
	}
	history, ok := service.(quotaHistoryService)
	if !ok {
		return nil, cpauk.ErrUnavailable
	}
	return history, nil
}

type quotaHistoryInput struct {
	AuthIndex   string     `json:"auth_index"`
	WindowID    string     `json:"window_id"`
	Remaining   *float64   `json:"remaining_percent"`
	ResetAt     *time.Time `json:"reset_at"`
	PeriodHours *float64   `json:"period_hours"`
	PlanID      string     `json:"plan_id"`
	Capacity    *float64   `json:"capacity"`
}

func (h *Handler) PostQuotaHistory(c *gin.Context) {
	data, err := readBoundedBody(c, 32*1024)
	if err != nil {
		writeAnalyticsInvalid(c, err)
		return
	}
	var body struct {
		Provider string              `json:"provider"`
		Samples  []quotaHistoryInput `json:"samples"`
	}
	if json.Unmarshal(data, &body) != nil || !validQuotaHistoryProvider(body.Provider) || len(body.Samples) == 0 || len(body.Samples) > 100 {
		writeAnalyticsInvalid(c, nil)
		return
	}
	service, err := h.quotaHistoryService()
	if err != nil {
		writeAnalyticsError(c, err)
		return
	}
	samples := make([]store.QuotaHistorySample, 0, len(body.Samples))
	for _, input := range body.Samples {
		if len(input.WindowID) > 64 || len(input.PlanID) > 64 || !quotaHistoryIdentifier.MatchString(input.WindowID) || input.PlanID != "" && !quotaHistoryIdentifier.MatchString(input.PlanID) || input.Remaining == nil || !validQuotaHistoryNumber(input.Remaining, 100) || !validQuotaHistoryNumber(input.Capacity, 1e12) || !validQuotaHistoryNumber(input.PeriodHours, 24*366) {
			writeAnalyticsInvalid(c, nil)
			return
		}
		auth := h.authByIndex(input.AuthIndex)
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled || h.quotaHistoryProvider(c.Request.Context(), auth) != body.Provider {
			writeAnalyticsInvalid(c, nil)
			return
		}
		id, errID := service.CredentialID(body.Provider, auth.Index, auth.ID)
		if errID != nil {
			writeAnalyticsError(c, errID)
			return
		}
		if id == nil {
			writeAnalyticsError(c, cpauk.ErrUnavailable)
			return
		}
		samples = append(samples, store.QuotaHistorySample{Provider: body.Provider, CredentialID: *id, WindowID: input.WindowID, RemainingPercent: *input.Remaining, ResetAt: input.ResetAt, PeriodHours: input.PeriodHours, PlanID: input.PlanID, Capacity: input.Capacity, Source: "cpamc"})
	}
	if err = service.RecordQuotaHistory(c.Request.Context(), samples); err != nil {
		writeAnalyticsError(c, err)
		return
	}
	setAnalyticsNoStore(c)
	c.Status(http.StatusNoContent)
}

func (h *Handler) GetQuotaHistory(c *gin.Context) {
	provider := c.Query("provider")
	start, errStart := time.Parse(time.RFC3339Nano, c.Query("start"))
	end, errEnd := time.Parse(time.RFC3339Nano, c.Query("end"))
	if !validQuotaHistoryProvider(provider) || errStart != nil || errEnd != nil || !end.After(start) || end.Sub(start) > 90*24*time.Hour {
		writeAnalyticsInvalid(c, nil)
		return
	}
	service, err := h.quotaHistoryService()
	if err != nil {
		writeAnalyticsError(c, err)
		return
	}
	samples, err := service.QuotaHistory(c.Request.Context(), provider, start, end)
	if err != nil {
		writeAnalyticsError(c, err)
		return
	}
	var activeIDs []string
	var identities []gin.H
	if h.authManager != nil {
		activeIDs = []string{}
		identities = []gin.H{}
		for _, auth := range h.authManager.List() {
			if auth == nil || h.quotaHistoryProvider(c.Request.Context(), auth) != provider {
				continue
			}
			auth.EnsureIndex()
			id, errID := service.CredentialID(provider, auth.Index, auth.ID)
			if errID != nil {
				writeAnalyticsError(c, errID)
				return
			}
			if id != nil {
				activeIDs = append(activeIDs, *id)
				identities = append(identities, gin.H{"auth_index": auth.Index, "credential_id": *id})
			}
		}
	}
	setAnalyticsNoStore(c)
	c.JSON(http.StatusOK, gin.H{"samples": samples, "active_credential_ids": activeIDs, "credential_identities": identities})
}

// quotaHistoryProvider follows the collector identity first, then the built-in
// and plugin quota classifications exposed by the credential list.
func (h *Handler) quotaHistoryProvider(ctx context.Context, auth *coreauth.Auth) string {
	if provider := coreauth.QuotaRecoveryProviderForAuth(auth); provider != "" {
		return provider
	}
	provider := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(auth.Provider)), "_", "-")
	switch provider {
	case "antigravity", "devin", "meta":
		return provider
	case "kimi", "kimi-ai":
		return "kimi"
	case "xai", "x-ai", "grok":
		return "xai"
	}
	h.mu.Lock()
	host := h.pluginHost
	h.mu.Unlock()
	if host != nil && host.QuotaProviderPluginIDs(ctx)[strings.ToLower(strings.TrimSpace(auth.Provider))] != "" {
		return "plugin"
	}
	return ""
}
