package management

import (
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

const managementScopeContextKey = "management.scope"

func (h *Handler) GetManagementSession(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"scope": c.GetString(managementScopeContextKey)})
}

// readOnlyManagementRoute deliberately rejects unknown routes, legacy routes,
// plugin dispatch, config subpaths, credential downloads, and GET operations that start work.
func readOnlyManagementRoute(c *gin.Context) bool {
	path := strings.TrimPrefix(c.FullPath(), "/v8/management")
	if path == c.FullPath() {
		return false
	}
	if c.Request.Method == http.MethodPost {
		return path == "/analytics/query" || path == "/analytics/exports" || path == "/telemetry/query" || path == "/requests/api-call" || path == "/plugins/:id/quota"
	}
	if c.Request.Method != http.MethodGet {
		return false
	}
	switch path {
	case "/overview", "/server/latest-version", "/observability/logs", "/observability/logs/errors",
		"/observability/logs/errors/:name", "/observability/logs/requests/:id", "/observability/usage/api-keys",
		"/plugins/:id/quota", "/session", "/capabilities", "/storage", "/credentials", "/credentials/models",
		"/routing/model-definitions/:channel", "/telemetry/health", "/telemetry/schema",
		"/analytics/health", "/analytics/summary", "/analytics/timeseries", "/analytics/dimensions",
		"/analytics/events", "/analytics/events/:attempt_id", "/analytics/pricing",
		"/analytics/pricing/catalog-providers", "/analytics/providers", "/analytics/quotas",
		"/analytics/quota-history", "/analytics/keys", "/analytics/leaderboard", "/analytics/query":
		return true
	}
	return false
}

func (h *Handler) ListManagementKeys(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := append([]config.ManagementKey{}, h.cfg.RemoteManagement.Keys...)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"keys": keys})
}

func (h *Handler) CreateManagementKey(c *gin.Context) { h.writeManagementKey(c, false) }
func (h *Handler) PatchManagementKey(c *gin.Context)  { h.writeManagementKey(c, true) }

func (h *Handler) writeManagementKey(c *gin.Context, patch bool) {
	var body struct {
		Name  *string `json:"name"`
		Key   *string `json:"key"`
		Scope *string `json:"scope"`
	}
	if c.ShouldBindJSON(&body) != nil || (!patch && (body.Name == nil || body.Key == nil || body.Scope == nil)) ||
		(body.Name != nil && strings.TrimSpace(*body.Name) == "") || (body.Key != nil && (*body.Key == "" || len(*body.Key) > 72)) ||
		(body.Scope != nil && !config.ValidManagementScope(*body.Scope)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid management key"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Read the persisted model before editing: h.cfg can include runtime-owned fields
	// or lag a disk change. The ordinary config writer preserves unrelated settings.
	next, ok := h.loadManagementWriteConfigLocked(c)
	if !ok {
		return
	}
	index := -1
	entry := config.ManagementKey{ID: uuid.NewString()}
	if patch {
		for i, key := range next.RemoteManagement.Keys {
			if key.ID == c.Param("id") {
				index = i
				entry = key
				break
			}
		}
		if index == -1 {
			c.JSON(http.StatusNotFound, gin.H{"error": "management key not found"})
			return
		}
	}
	if body.Name != nil {
		entry.Name = strings.TrimSpace(*body.Name)
	}
	if body.Scope != nil {
		entry.Scope = *body.Scope
	}
	if body.Key != nil {
		hashed, err := bcrypt.GenerateFromPassword([]byte(*body.Key), bcrypt.DefaultCost)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid management key"})
			return
		}
		entry.SecretKey = string(hashed)
	}
	if patch {
		next.RemoteManagement.Keys[index] = entry
	} else {
		next.RemoteManagement.Keys = append(next.RemoteManagement.Keys, entry)
	}
	if !h.saveManagementKeysLocked(c, next) {
		return
	}
	c.JSON(http.StatusOK, entry)
}

func (h *Handler) DeleteManagementKey(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	next, ok := h.loadManagementWriteConfigLocked(c)
	if !ok {
		return
	}
	for i, key := range next.RemoteManagement.Keys {
		if key.ID == c.Param("id") {
			next.RemoteManagement.Keys = append(next.RemoteManagement.Keys[:i], next.RemoteManagement.Keys[i+1:]...)
			if h.saveManagementKeysLocked(c, next) {
				c.JSON(http.StatusOK, gin.H{"status": "ok"})
			}
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "management key not found"})
}

func (h *Handler) loadManagementWriteConfigLocked(c *gin.Context) (*config.Config, bool) {
	raw, err := os.ReadFile(h.configFilePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed"})
		return nil, false
	}
	next, err := config.ParseConfigBytes(raw)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid_config"})
		return nil, false
	}
	return next, true
}

func (h *Handler) saveManagementKeysLocked(c *gin.Context, next *config.Config) bool {
	raw, err := os.ReadFile(h.configFilePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed"})
		return false
	}
	raw, _, err = config.NormalizeConfigLayout(raw, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid_config"})
		return false
	}
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil || len(doc.Content) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid_config"})
		return false
	}
	root := doc.Content[0]
	management := configV8Node(root, []string{"management"})
	if management == nil {
		management = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "management"}, management)
	}
	var keys yaml.Node
	if keys.Encode(next.RemoteManagement.Keys) != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encode_failed"})
		return false
	}
	if current := configV8Node(management, []string{"keys"}); current != nil {
		// Keep comments and layout on surviving entries and scalar fields.
		for i, entry := range next.RemoteManagement.Keys {
			for _, old := range current.Content {
				id := configV8Node(old, []string{"id"})
				if id == nil || id.Value != entry.ID {
					continue
				}
				merged := cloneConfigV8Node(old)
				for _, field := range []string{"id", "name", "secret-key", "scope"} {
					value := configV8Node(keys.Content[i], []string{field})
					if previous := configV8Node(merged, []string{field}); previous != nil {
						previous.Value, previous.Tag = value.Value, value.Tag
					} else {
						merged.Content = append(merged.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: field}, value)
					}
				}
				keys.Content[i] = merged
				break
			}
		}
		keys.HeadComment, keys.LineComment, keys.FootComment = current.HeadComment, current.LineComment, current.FootComment
		*current = keys
	} else {
		management.Content = append(management.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "keys"}, &keys)
	}
	raw, err = yaml.Marshal(&doc)
	if err != nil || WriteConfig(h.configFilePath, raw) != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed"})
		return false
	}
	next.Home = h.cfg.Home
	h.cfg = next
	h.reloadConfigAfterManagementSaveAsync(c.Request.Context(), h.reloadSnapshotConfigLocked())
	return true
}

func sanitizeReadOnlyCredentials(c *gin.Context, files []gin.H) {
	if !isReadOnlyManagement(c) {
		return
	}
	// Preserve quota identifiers and observations without exposing arbitrary
	// provider-owned metadata, error messages, bypass API keys, or filesystem paths.
	allowed := map[string]bool{}
	for _, field := range []string{"id", "auth_index", "name", "display_name", "type", "provider", "label", "status", "disabled", "unavailable", "runtime_only", "source", "size", "success", "failed", "recent_requests", "quota", "quota_reserve", "quota_reserve_state", "model_quotas", "supports_quota", "quota_provider", "quota_plugin_id", "email", "project_id", "profile_arn", "account_type", "account", "created_at", "updated_at", "modtime", "last_refresh", "next_retry_after", "cooldowns", "id_token", "usage_probe", "pricing_catalog"} {
		allowed[field] = true
	}
	for _, entry := range files {
		if entry["account_type"] == "api_key" {
			delete(entry, "account")
		}
		for key := range entry {
			if !allowed[key] {
				delete(entry, key)
			}
		}
	}
}
