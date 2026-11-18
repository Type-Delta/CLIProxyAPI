package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/livehandle"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func (h *Handler) GetLiveRequests(c *gin.Context) {
	snapshot := livehandle.Default.Snapshot()
	h.decorateLiveRequests(&snapshot)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, snapshot)
}

// decorateLiveRequests adds current display names: API-key labels from the config, and credential
// labels from the auth manager, so a rename shows on requests already in flight.
func (h *Handler) decorateLiveRequests(snapshot *livehandle.Snapshot) {
	// Copied under the lock: management writes replace the key list in place.
	labels := make(map[string]string)
	h.mu.Lock()
	if h.cfg != nil {
		for _, entry := range h.cfg.APIKeys {
			labels[config.APIKeyID(entry.Key)] = entry.Label
		}
	}
	manager := h.authManager
	h.mu.Unlock()
	for i := range snapshot.Requests {
		req := &snapshot.Requests[i]
		if len(req.KeyID) >= 8 {
			req.Key = &livehandle.Key{ID: req.KeyID[:8], Label: labels[req.KeyID]}
		}
		if manager == nil {
			continue
		}
		for j := range req.Attempts {
			attempt := &req.Attempts[j]
			if auth, ok := manager.GetByID(attempt.AuthID); ok && attempt.AuthID != "" {
				attempt.Credential.Label = coreauth.CredentialLabel(auth)
			}
		}
	}
}

// managementServing reports whether the server still serves the Management API at all.
func (h *Handler) managementServing() bool {
	h.mu.Lock()
	available := h.managementAvailable
	h.mu.Unlock()
	return available == nil || available()
}

// StreamLiveRequests follows the live registry over SSE. The caller is checked again at open and
// whenever management access settings change, and the stream ends before its next frame once the
// key is revoked or the Management API stops serving (Home mode, no management secret).
func (h *Handler) StreamLiveRequests(c *gin.Context) {
	clientIP := c.ClientIP()
	localClient := clientIP == "127.0.0.1" || clientIP == "::1"
	provided := providedManagementKey(c)
	// The baseline is taken before the check below, so a revocation that lands after the
	// middleware let this caller in is still seen.
	authState := h.managementAuthState()
	if _, allowed, status, message := h.checkManagementScope(clientIP, localClient, provided, false); !allowed {
		c.AbortWithStatusJSON(status, gin.H{"error": message})
		return
	}
	frames, unsubscribe := livehandle.Default.Subscribe(h.decorateLiveRequests)
	defer unsubscribe()
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Accel-Buffering", "no")
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case frame, ok := <-frames:
			if !ok {
				return
			}
			if !h.managementServing() {
				return
			}
			if state := h.managementAuthState(); state != authState {
				if _, allowed, _, _ := h.checkManagementScope(clientIP, localClient, provided, false); !allowed {
					return
				}
				authState = state
			}
			if _, err := c.Writer.Write(frame); err != nil {
				return
			}
			c.Writer.Flush()
		}
	}
}
