package management

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagelimit"
)

type apiKeyLimitsEntry struct {
	Key    string               `json:"key"`
	Limits *usagelimit.Snapshot `json:"limits"`
}

// GetAPIKeyLimits returns configured inbound API key limits and current usage.
func (h *Handler) GetAPIKeyLimits(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	h.mu.Lock()
	tracker := h.usageLimitTracker
	h.mu.Unlock()
	if tracker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage limit tracker unavailable"})
		return
	}

	now := time.Now()
	keys := tracker.Keys()
	entries := make([]apiKeyLimitsEntry, 0, len(keys))
	for _, key := range keys {
		limits := tracker.Snapshot(key, now)
		if limits == nil {
			continue
		}
		entries = append(entries, apiKeyLimitsEntry{
			Key:    key,
			Limits: limits,
		})
	}
	c.JSON(http.StatusOK, gin.H{"api-key-limits": entries})
}
