package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentdocs"
)

const agentDocsLink = `</llms.txt>; rel="describedby"`

// unknownManagementRouteBody answers unknown management paths. The error text repeats the docs
// path because agents often keep only the error field.
var unknownManagementRouteBody = gin.H{"error": "unknown management route; see /llms.txt for the management API", "agent_docs": "/llms.txt"}

func isAgentDocsPath(path string) bool {
	return path == "/llms.txt" || path == "/llms/management.md" || path == "/llms/sql.md"
}

// agentDocsDiscoveryMiddleware runs before access checks, including v8 authentication.
func (s *Server) agentDocsDiscoveryMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if s.managementEnabled() && (path == "/" || path == "/v8/management" || strings.HasPrefix(path, "/v8/management/") || isAgentDocsPath(path)) {
			c.Writer.Header().Add("Link", agentDocsLink)
		}
		c.Next()
	}
}

func (s *Server) registerAgentDocsRoutes() {
	for _, path := range []string{"/llms.txt", "/llms/management.md", "/llms/sql.md"} {
		s.engine.GET(path, s.serveAgentDocs)
		s.engine.HEAD(path, s.serveAgentDocs)
	}
}

func (s *Server) serveAgentDocs(c *gin.Context) {
	if !s.managementAvailable(c) {
		return
	}
	name := strings.TrimPrefix(c.Request.URL.Path, "/llms/")
	if name == "/llms.txt" {
		name = "llms.txt"
	}
	data, err := agentdocs.Files.ReadFile(name)
	if err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	contentType := "text/markdown; charset=utf-8"
	if name == "llms.txt" {
		contentType = "text/plain; charset=utf-8"
	}
	c.Header("Content-Type", contentType)
	c.Header("Content-Length", strconv.Itoa(len(data)))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Data(http.StatusOK, contentType, data)
}
