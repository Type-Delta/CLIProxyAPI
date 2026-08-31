package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagelimit"
)

type apiKeyLimitsEntry struct {
	Key            string               `json:"key"`
	KeyID          string               `json:"key_id"`
	ConfigIndex    int                  `json:"config_index"`
	ConfigRevision string               `json:"config_revision"`
	Limits         *usagelimit.Snapshot `json:"limits"`
}

// GetAPIKeyLimits returns configured inbound API key limits and current usage.
func (h *Handler) GetAPIKeyLimits(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	h.mu.Lock()
	tracker := h.usageLimitTracker
	var configured []config.APIKeyEntry
	if h.cfg != nil {
		configured = append([]config.APIKeyEntry(nil), h.cfg.APIKeys...)
	}
	h.mu.Unlock()
	if tracker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage limit tracker unavailable"})
		return
	}

	now := time.Now()
	revision := apiKeyRevision(configured)
	keys := tracker.Keys()
	entries := make([]apiKeyLimitsEntry, 0, len(keys))
	for _, key := range keys {
		limits := tracker.Snapshot(key, now)
		if limits == nil {
			continue
		}
		configIndex := firstAPIKeyConfigIndex(configured, key)
		entries = append(entries, apiKeyLimitsEntry{
			Key:            key,
			KeyID:          config.APIKeyID(key),
			ConfigIndex:    configIndex,
			ConfigRevision: revision,
			Limits:         limits,
		})
	}
	setAPIKeyRevisionHeaders(c, revision, true)
	c.JSON(http.StatusOK, gin.H{"api-key-limits": entries})
}

func firstAPIKeyConfigIndex(entries []config.APIKeyEntry, raw string) int {
	raw = strings.TrimSpace(raw)
	for index, entry := range entries {
		if strings.TrimSpace(entry.Key) == raw {
			return index
		}
	}
	return -1
}

// ResetAPIKeyLimits clears the current usage for one configured inbound API key.
func (h *Handler) ResetAPIKeyLimits(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	var body struct {
		Key   string `json:"key"`
		KeyID string `json:"key_id"`
	}
	if errBind := c.ShouldBindJSON(&body); errBind != nil || (strings.TrimSpace(body.Key) == "" && strings.TrimSpace(body.KeyID) == "") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	h.mu.Lock()
	tracker := h.usageLimitTracker
	configured := false
	key := ""
	requestedKey := strings.TrimSpace(body.Key)
	requestedKeyID := strings.TrimSpace(body.KeyID)
	if h.cfg != nil {
		for _, entry := range h.cfg.APIKeys {
			candidate := strings.TrimSpace(entry.Key)
			if candidate == "" {
				continue
			}
			if requestedKeyID != "" && config.APIKeyID(candidate) == requestedKeyID {
				if key != "" && key != candidate {
					h.mu.Unlock()
					c.JSON(http.StatusConflict, gin.H{"error": "api_key_identity_conflict"})
					return
				}
				key = candidate
				configured = true
			}
			if requestedKeyID == "" && candidate == requestedKey {
				key = candidate
				configured = true
			}
		}
	}
	h.mu.Unlock()
	if configured && requestedKey != "" && requestedKey != key {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key and key_id do not identify the same API key"})
		return
	}
	if !configured {
		c.JSON(http.StatusNotFound, gin.H{"error": "API key limit not found"})
		return
	}
	if tracker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage limit tracker unavailable"})
		return
	}
	tracker.Reset(key)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "key_id": config.APIKeyID(key)})
}
