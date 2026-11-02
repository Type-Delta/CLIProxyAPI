package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"gopkg.in/yaml.v3"
)

var readOnlyHeaderPattern = regexp.MustCompile(`(?im)^([^\r\n:]*?(?:authorization|cookie|api[-_]?key|token|secret)[^\r\n:]*:\s*)[^\r\n]*`)

// readOnlyLogContent omits arbitrary request payloads, which can contain secrets
// under any field name. Application diagnostics retain text with credentials removed.
func (h *Handler) readOnlyLogContent(c *gin.Context, content string, request bool) string {
	if !isReadOnlyManagement(c) {
		return content
	}
	if request {
		var lines []string
		section := ""
		firstSection := true
		initialMetadata := false
		for _, line := range strings.Split(content, "\n") {
			if strings.HasPrefix(line, "=== ") {
				section = line
				initialMetadata = firstSection && line == "=== REQUEST INFO ==="
				firstSection = false
				if initialMetadata || line == "=== RESPONSE ===" {
					lines = append(lines, line)
				}
				continue
			}
			allowed := false
			if initialMetadata {
				for _, field := range []string{"Version:", "URL:", "Method:", "Timestamp:", "Downstream Transport:", "Upstream Transport:"} {
					if strings.HasPrefix(line, field) {
						allowed = true
					}
				}
				if strings.HasPrefix(line, "URL:") {
					if u, err := url.Parse(strings.TrimSpace(strings.TrimPrefix(line, "URL:"))); err == nil {
						u.User = nil
						u.RawQuery = ""
						u.Fragment = ""
						line = "URL: " + u.String()
					} else {
						line = "URL: [REDACTED]"
					}
				}
			}
			if section == "=== RESPONSE ===" && regexpStatusLine.MatchString(line) {
				allowed = true
			}
			if allowed {
				lines = append(lines, line)
			}
		}
		lines = append(lines, "[Headers and payloads omitted for read-only access]")
		content = strings.Join(lines, "\n")
	}
	// Reuse the logger's credential assignment and URL redaction without its
	// diagnostic truncation. Also remove known credentials appearing as plain text.
	content = readOnlyURLPattern.ReplaceAllStringFunc(content, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "[REDACTED URL]"
		}
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		return u.String()
	})
	content = readOnlyCookiePattern.ReplaceAllString(content, `${1}"[REDACTED]"`)
	content = logging.RedactCredentialValues(content)
	content = readOnlyHeaderPattern.ReplaceAllString(content, `${1}[REDACTED]`)
	secrets := map[string]bool{strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "): true, c.GetHeader("X-Management-Key"): true}
	var collect func(any, bool)
	collect = func(value any, sensitive bool) {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				key = strings.ToLower(strings.ReplaceAll(key, "_", "-"))
				secret := sensitive || strings.Contains(key, "key") || strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(key, "password") || strings.Contains(key, "header") || strings.Contains(key, "credential") || strings.Contains(key, "cookie")
				collect(child, secret)
			}
		case []any:
			for _, child := range v {
				collect(child, sensitive)
			}
		case map[any]any:
			for key, child := range v {
				collect(map[string]any{fmt.Sprint(key): child}, sensitive)
			}
		case string:
			if sensitive && v != "" {
				secrets[v] = true
			}
			if u, err := url.Parse(v); err == nil && u.Host != "" {
				if u.User != nil {
					secrets[u.User.Username()] = true
					if password, ok := u.User.Password(); ok {
						secrets[password] = true
					}
				}
				for _, values := range u.Query() {
					for _, value := range values {
						if value != "" {
							secrets[value] = true
						}
					}
				}
			}
		}
	}
	h.mu.Lock()
	raw, _ := json.Marshal(h.cfg)
	var pluginRaw []byte
	if h.cfg != nil {
		pluginRaw, _ = yaml.Marshal(h.cfg.Plugins.Configs)
		secrets[h.cfg.RemoteManagement.SecretKey] = true
		for _, key := range h.cfg.RemoteManagement.Keys {
			secrets[key.SecretKey] = true
		}
	}
	manager := h.authManager
	secrets[h.envSecret] = true
	secrets[h.localPassword] = true
	h.mu.Unlock()
	var value any
	if json.Unmarshal(raw, &value) == nil {
		collect(value, false)
	}
	if yaml.Unmarshal(pluginRaw, &value) == nil {
		collect(value, false)
	}
	if manager != nil {
		for _, auth := range manager.List() {
			if auth == nil {
				continue
			}
			for _, secret := range []string{auth.RejectedAccessToken, auth.RejectedSIWCAccessToken, auth.RejectedRefreshToken, auth.RejectedSIWCRefreshToken, auth.RenewalWarnedRefreshToken} {
				secrets[secret] = true
			}
			raw, _ := json.Marshal(auth)
			if json.Unmarshal(raw, &value) == nil {
				collect(value, false)
			}
		}
	}
	values := make([]string, 0, len(secrets))
	for secret := range secrets {
		if secret != "" {
			values = append(values, secret)
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, secret := range values {
		content = strings.ReplaceAll(content, secret, "[REDACTED]")
		encoded, _ := json.Marshal(secret)
		content = strings.ReplaceAll(content, string(encoded[1:len(encoded)-1]), "[REDACTED]")
		content = strings.ReplaceAll(content, url.QueryEscape(secret), "[REDACTED]")
	}
	return content
}

var readOnlyURLPattern = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
var readOnlyCookiePattern = regexp.MustCompile(`(?i)(["']?(?:cookie|set-cookie)["']?\s*[:=]\s*)(?:"(?:\\.|[^"])*"|'(?:\\.|[^'])*'|[^\r\n,;}]+)`)

var regexpStatusLine = regexp.MustCompile(`^Status: [1-5][0-9]{2}$`)

func (h *Handler) writeLogsResponse(c *gin.Context, lines []string, count int, latest int64, cursor string, reset bool) {
	if isReadOnlyManagement(c) {
		c.Header("Cache-Control", "no-store")
	}
	if isReadOnlyManagement(c) && len(lines) > 0 {
		lines = strings.Split(h.readOnlyLogContent(c, strings.Join(lines, "\n"), false), "\n")
	}
	writeLogsResponse(c, lines, count, latest, cursor, reset)
}

func (h *Handler) serveManagementLogFile(c *gin.Context, path, name string) {
	if !isReadOnlyManagement(c) {
		c.FileAttachment(path, name)
		return
	}
	content, err := os.ReadFile(path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read log file"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(h.readOnlyLogContent(c, string(content), true)))
}
