package management

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"gopkg.in/yaml.v3"
)

const (
	apiKeyContractHeader = "X-CPA-API-Key-Contract"
	apiKeyContractV1     = "1"
)

func apiKeyRevision(entries []config.APIKeyEntry) string {
	return config.APIKeyConfigRevision(entries)
}

func setAPIKeyRevisionHeaders(c *gin.Context, revision string, raw bool) {
	c.Header("ETag", `"`+revision+`"`)
	c.Header("X-CPA-Config-Revision", revision)
	if raw {
		c.Header("Cache-Control", "no-store")
		c.Header("Referrer-Policy", "no-referrer")
	}
}

func apiKeyContractEnabled(c *gin.Context) bool {
	return strings.TrimSpace(c.GetHeader(apiKeyContractHeader)) == apiKeyContractV1
}

func requestAPIKeyRevision(c *gin.Context, bodyRevision string) string {
	if revision := strings.TrimSpace(bodyRevision); revision != "" {
		return trimETag(revision)
	}
	if revision := strings.TrimSpace(c.GetHeader("If-Match")); revision != "" {
		return trimETag(revision)
	}
	return strings.TrimSpace(c.Query("config_revision"))
}

func trimETag(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "W/") {
		value = strings.TrimSpace(strings.TrimPrefix(value, "W/"))
	}
	return strings.Trim(value, `"`)
}

func guardAPIKeyRevision(c *gin.Context, current []config.APIKeyEntry, bodyRevision string) bool {
	return guardAPIKeyRevisionMode(c, current, bodyRevision, apiKeyContractEnabled(c))
}

func guardRequiredAPIKeyRevision(c *gin.Context, current []config.APIKeyEntry, bodyRevision string) bool {
	return guardAPIKeyRevisionMode(c, current, bodyRevision, true)
}

func guardAPIKeyRevisionMode(c *gin.Context, current []config.APIKeyEntry, bodyRevision string, required bool) bool {
	want := requestAPIKeyRevision(c, bodyRevision)
	if want == "" {
		if !required {
			return false
		}
		c.JSON(http.StatusConflict, gin.H{
			"error":   "config_revision_required",
			"message": "A configuration revision is required for this API-key update.",
		})
		return true
	}
	currentRevision := apiKeyRevision(current)
	if want != currentRevision {
		setAPIKeyRevisionHeaders(c, currentRevision, false)
		c.JSON(http.StatusConflict, gin.H{
			"error":   "config_revision_mismatch",
			"message": "The API-key configuration changed. Reload it before saving.",
		})
		return true
	}
	return false
}

func guardLegacyStructuredAPIKeyMutation(c *gin.Context, current, candidate []config.APIKeyEntry) bool {
	if apiKeyContractEnabled(c) || sameAPIKeyEntries(current, candidate) {
		return false
	}
	for _, entry := range current {
		if entry.IsStructured() {
			c.JSON(http.StatusConflict, gin.H{
				"error":   "structured_api_keys_required",
				"message": "This API-key update requires the structured contract.",
			})
			return true
		}
	}
	return false
}

func sameAPIKeyEntries(left, right []config.APIKeyEntry) bool {
	leftJSON, errLeft := json.Marshal(left)
	rightJSON, errRight := json.Marshal(right)
	return errLeft == nil && errRight == nil && reflect.DeepEqual(leftJSON, rightJSON)
}

// GuardConfigYAMLAPIKeyMutation applies the API-key compatibility and revision
// checks to PUT /config.yaml before typed decoding can discard extension fields.
// It returns true after writing a rejection response.
func (h *Handler) GuardConfigYAMLAPIKeyMutation(c *gin.Context, candidateYAML []byte) bool {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return true
	}
	candidate, ok := decodeConfigYAMLAPIKeys(candidateYAML)
	if !ok {
		return false
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	return h.guardConfigYAMLAPIKeyMutationLocked(c, candidate)
}

// GuardConfigYAMLAPIKeyMutationLocked performs the final compatibility and
// revision check immediately before a full config write. The caller must hold
// h.mu through both this call and WriteConfig so another management mutation or
// hot reload cannot invalidate the accepted revision.
func (h *Handler) GuardConfigYAMLAPIKeyMutationLocked(c *gin.Context, candidateYAML []byte) bool {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return true
	}
	candidate, ok := decodeConfigYAMLAPIKeys(candidateYAML)
	if !ok {
		return false
	}
	return h.guardConfigYAMLAPIKeyMutationLocked(c, candidate)
}

func (h *Handler) guardConfigYAMLAPIKeyMutationLocked(c *gin.Context, candidate []config.APIKeyEntry) bool {
	var current []config.APIKeyEntry
	if h.cfg != nil {
		current = h.cfg.APIKeys
	}
	if sameAPIKeyEntries(current, candidate) {
		return false
	}
	if errDuplicate := config.ValidateAPIKeyMutation(current, candidate); errDuplicate != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "duplicate_api_key", "message": errDuplicate.Error()})
		return true
	}
	if guardLegacyStructuredAPIKeyMutation(c, current, candidate) {
		return true
	}
	return guardAPIKeyRevision(c, current, "")
}

func decodeConfigYAMLAPIKeys(candidateYAML []byte) ([]config.APIKeyEntry, bool) {
	var document struct {
		APIKeys []config.APIKeyEntry `yaml:"api-keys"`
	}
	if err := yaml.Unmarshal(candidateYAML, &document); err != nil {
		return nil, false
	}
	return document.APIKeys, true
}
