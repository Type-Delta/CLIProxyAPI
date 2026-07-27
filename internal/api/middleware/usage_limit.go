package middleware

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagelimit"
)

// Protocol selects the error body shape for rejections.
type Protocol string

const (
	ProtocolOpenAI    Protocol = "openai"
	ProtocolAnthropic Protocol = "anthropic"
	ProtocolGemini    Protocol = "gemini"
)

// UsageLimitMiddleware gates requests against per-API-key usage limits.
func UsageLimitMiddleware(tracker *usagelimit.Tracker, protocol Protocol) gin.HandlerFunc {
	return func(c *gin.Context) {
		if tracker == nil {
			c.Next()
			return
		}
		if usageLimitRouteIsSkipped(c) {
			c.Next()
			return
		}

		key := c.GetString("userApiKey")
		if key == "" {
			c.Next()
			return
		}

		now := time.Now()
		decision := tracker.Allow(key, now)
		if decision.Allowed {
			c.Next()
			return
		}

		setUsageLimitHeaders(c, decision, now)
		c.AbortWithStatusJSON(http.StatusTooManyRequests, usageLimitErrorBody(protocol, decision))
	}
}

func usageLimitRouteIsSkipped(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}

	switch c.Request.Method + " " + c.FullPath() {
	case "GET /v1/models",
		"GET /v1beta/models",
		"POST /v1/messages/count_tokens",
		"GET /v1/videos/:request_id",
		"GET /openai/v1/videos/:video_id",
		"GET /openai/v1/videos/:video_id/content":
		return true
	default:
		return false
	}
}

func setUsageLimitHeaders(c *gin.Context, decision usagelimit.Decision, now time.Time) {
	c.Header("X-RateLimit-Limit", strconv.FormatInt(decision.Limit, 10))
	c.Header("X-RateLimit-Remaining", "0")
	if decision.Resets == usagelimit.PeriodLifetime || decision.ResetAt == nil {
		return
	}

	retryAfter := int64(math.Ceil(decision.ResetAt.Sub(now).Seconds()))
	if retryAfter < 1 {
		retryAfter = 1
	}
	c.Header("Retry-After", strconv.FormatInt(retryAfter, 10))
	c.Header("X-RateLimit-Reset", strconv.FormatInt(decision.ResetAt.Unix(), 10))
}

func usageLimitErrorBody(protocol Protocol, decision usagelimit.Decision) gin.H {
	message := fmt.Sprintf("usage limit exceeded: %s limit of %d reached", decision.Metric, decision.Limit)
	if decision.Resets != usagelimit.PeriodLifetime && decision.ResetAt != nil {
		message += "; resets at " + decision.ResetAt.UTC().Format(time.RFC3339)
	}

	switch protocol {
	case ProtocolAnthropic:
		return gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "rate_limit_error",
				"message": message,
			},
		}
	case ProtocolGemini:
		return gin.H{
			"error": gin.H{
				"code":    http.StatusTooManyRequests,
				"message": message,
				"status":  "RESOURCE_EXHAUSTED",
			},
		}
	default:
		return gin.H{
			"error": gin.H{
				"message": message,
				"type":    "rate_limit_exceeded",
				"code":    "usage_limit_exceeded",
			},
		}
	}
}
