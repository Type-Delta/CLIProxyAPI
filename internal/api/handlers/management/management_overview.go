package management

import (
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
)

// GetManagementOverview exposes only the settings needed by observational pages.
func (h *Handler) GetManagementOverview(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cfg := h.cfg
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	counts := map[string]int{"gemini": len(cfg.GeminiKey), "interactions": len(cfg.InteractionsKey), "codex": len(cfg.CodexKey), "meta": len(cfg.MetaKey), "xai": len(cfg.XAIKey), "claude": len(cfg.ClaudeKey), "vertex": len(cfg.VertexCompatAPIKey), "openai": 0}
	for _, provider := range cfg.OpenAICompatibility {
		counts["openai"] += len(provider.APIKeyEntries)
	}
	total := 0
	for _, count := range counts {
		total += count
	}
	proxy := ""
	if u, err := url.Parse(cfg.ProxyURL); err == nil && u.Hostname() != "" {
		proxy = u.Scheme + "://" + u.Host
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"config": gin.H{"debug": cfg.Debug, "loggingToFile": cfg.LoggingToFile, "requestLog": cfg.RequestLog, "requestRetry": cfg.RequestRetry, "routingStrategy": cfg.Routing.Strategy, "proxyUrl": proxy, "quotaExceeded": gin.H{"switchProject": cfg.QuotaExceeded.SwitchProject, "switchPreviewModel": cfg.QuotaExceeded.SwitchPreviewModel}},
		"counts": gin.H{"apiKeys": len(cfg.APIKeys), "providerKeys": total}, "providerKeyCounts": counts,
	})
}
