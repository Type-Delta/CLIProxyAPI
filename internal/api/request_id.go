package api

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagecontext"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const proxyRequestIDGinKey = "__proxy_request_id__"

func init() {
	usagecontext.Install()
}

// ProxyRequestIDMiddleware assigns one 128-bit correlation ID to the request.
// Every upstream attempt inherits it through the request context.
func ProxyRequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil || c.Request == nil {
			return
		}
		ctx, requestID := coreusage.EnsureProxyRequestID(c.Request.Context())
		ctx = coreusage.WithEndpointClass(ctx, usageEndpointClass(c.Request.URL.Path))
		c.Set(proxyRequestIDGinKey, requestID)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func usageEndpointClass(path string) string {
	path = strings.ToLower(strings.TrimSpace(path))
	switch {
	case strings.Contains(path, "/chat/completions"):
		return "chat_completions"
	case strings.Contains(path, "/responses"):
		return "responses"
	case strings.Contains(path, "/messages"):
		return "messages"
	case strings.Contains(path, "/embeddings"):
		return "embeddings"
	case strings.Contains(path, "/images"):
		return "images"
	case strings.Contains(path, "/audio"):
		return "audio"
	case strings.Contains(path, "/videos"):
		return "videos"
	case strings.Contains(path, "/moderations"):
		return "moderations"
	case strings.Contains(path, "/realtime"):
		return "realtime"
	case strings.Contains(path, "/live"):
		return "live"
	case strings.Contains(path, "/alpha/search"):
		return "search"
	case strings.Contains(path, ":streamgeneratecontent"):
		return "gemini_stream_generate"
	case strings.Contains(path, ":generatecontent"):
		return "gemini_generate"
	case strings.HasSuffix(path, "/models"):
		return "models"
	default:
		return "other"
	}
}

// ProxyRequestIDFromGin returns the request correlation ID assigned by the middleware.
func ProxyRequestIDFromGin(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if value, ok := c.Get(proxyRequestIDGinKey); ok {
		if requestID, okString := value.(string); okString && coreusage.ValidProxyRequestID(requestID) {
			return requestID
		}
	}
	if c.Request == nil {
		return ""
	}
	return coreusage.ProxyRequestIDFromContext(c.Request.Context())
}
