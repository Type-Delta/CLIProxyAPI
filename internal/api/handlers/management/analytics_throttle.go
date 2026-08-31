package management

import (
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const analyticsAdminLimiterEntries = 4096

type analyticsAdminRateEntry struct {
	window time.Time
	count  int
}

type analyticsAdminRateLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]analyticsAdminRateEntry
}

func newAnalyticsAdminRateLimiter() *analyticsAdminRateLimiter {
	return &analyticsAdminRateLimiter{now: time.Now, entries: make(map[string]analyticsAdminRateEntry)}
}

func (l *analyticsAdminRateLimiter) allow(key string, limit int, window time.Duration) bool {
	if l == nil || limit < 1 || window <= 0 {
		return false
	}
	now := l.now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, exists := l.entries[key]
	if !exists || now.Sub(entry.window) >= window {
		if !exists && len(l.entries) >= analyticsAdminLimiterEntries {
			for candidate, value := range l.entries {
				if now.Sub(value.window) >= window {
					delete(l.entries, candidate)
				}
			}
			if len(l.entries) >= analyticsAdminLimiterEntries {
				return false
			}
		}
		entry = analyticsAdminRateEntry{window: now}
	}
	if entry.count >= limit {
		return false
	}
	entry.count++
	l.entries[key] = entry
	return true
}

// AnalyticsRateLimitMiddleware bounds only the admin reads that can fan out or
// scan substantial analytics history. Management authentication runs first.
func (h *Handler) AnalyticsRateLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := 0
		switch c.FullPath() {
		case "/v0/management/analytics/query":
			limit = 120
		case "/v0/management/analytics/exports":
			limit = 20
		case "/v0/management/analytics/events/:attempt_id":
			limit = 240
		}
		if limit == 0 {
			c.Next()
			return
		}
		h.mu.Lock()
		limiter := h.analyticsAdminLimiter
		h.mu.Unlock()
		if limiter == nil || !limiter.allow(analyticsImmediatePeer(c.Request.RemoteAddr)+"\x00"+c.FullPath(), limit, time.Minute) {
			c.Header("Retry-After", "60")
			WriteAnalyticsThrottled(c)
			return
		}
		c.Next()
	}
}

func analyticsImmediatePeer(remoteAddress string) string {
	remoteAddress = strings.TrimSpace(remoteAddress)
	if host, _, err := net.SplitHostPort(remoteAddress); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(remoteAddress, "[]")
}
