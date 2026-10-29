package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/credentialweight"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
)

var routingClientTypes = []string{"claude", "codex", "opencode", "pi", "agy", "gemini", "generic"}

type routingCredential struct {
	ID        string                  `json:"id"`
	Label     string                  `json:"label"`
	Provider  string                  `json:"provider"`
	Kind      string                  `json:"kind"`
	Priority  int                     `json:"priority"`
	Weight    int64                   `json:"weight"`
	Disabled  bool                    `json:"disabled"`
	Prefix    string                  `json:"prefix"`
	Cooldowns []coreauth.CooldownView `json:"cooldowns"`
}

type routingCredentialsResponse struct {
	ObservedAt  time.Time           `json:"observed_at"`
	HomeMode    bool                `json:"home_mode"`
	Credentials []routingCredential `json:"credentials"`
	ClientTypes []string            `json:"client_types"`
}

// GetRoutingCredentials returns the secret-free credential catalogue used by routing rules.
func (h *Handler) GetRoutingCredentials(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	observedAt := time.Now().UTC()
	homeMode := h.authManager.HomeEnabled()
	auths := h.authManager.List()
	credentials := make([]routingCredential, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		kind := "oauth"
		if auth.AuthKind() == coreauth.AuthKindAPIKey {
			kind = "api-key"
		}
		weight, hasWeight := authWeightValue(auth)
		if !hasWeight {
			weight = credentialweight.Default
		}
		var cooldowns []coreauth.CooldownView
		if !homeMode {
			cooldowns = coreauth.CooldownSnapshotForAuth(auth, observedAt)
		}
		credentials = append(credentials, routingCredential{
			ID:        auth.ID,
			Label:     routingCredentialLabel(auth),
			Provider:  auth.Provider,
			Kind:      kind,
			Priority:  routingCredentialPriority(auth),
			Weight:    weight,
			Disabled:  auth.Disabled || auth.Status == coreauth.StatusDisabled,
			Prefix:    auth.Prefix,
			Cooldowns: cooldowns,
		})
	}
	sort.Slice(credentials, func(i, j int) bool {
		if credentials[i].Provider != credentials[j].Provider {
			return credentials[i].Provider < credentials[j].Provider
		}
		return credentials[i].ID < credentials[j].ID
	})
	c.JSON(http.StatusOK, routingCredentialsResponse{
		HomeMode:    homeMode,
		ObservedAt:  observedAt,
		Credentials: credentials,
		ClientTypes: append([]string(nil), routingClientTypes...),
	})
}

type routingDryRunInput struct {
	Model      string            `json:"model"`
	APIKey     string            `json:"api_key_label"`
	ClientType string            `json:"client_type"`
	Headers    map[string]string `json:"headers"`
	SessionID  string            `json:"session_id"`
	Rules      json.RawMessage   `json:"rules"`
}

// PostRoutingDryRun simulates one routing decision without changing runtime state.
func (h *Handler) PostRoutingDryRun(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	data, err := readBoundedBody(c, 1<<20)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	var input routingDryRunInput
	if err = json.Unmarshal(data, &input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if strings.TrimSpace(input.Model) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is required", "path": "model"})
		return
	}

	cfg := h.routingConfigSnapshot()
	keyID, found, ambiguous := routingAPIKeyID(cfg, input.APIKey)
	if ambiguous {
		c.JSON(http.StatusBadRequest, gin.H{"error": "api_key_label is ambiguous", "path": "api_key_label"})
		return
	}
	if !found {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown api_key_label", "path": "api_key_label"})
		return
	}

	warnings := make([]string, 0)
	var draft *routing.RuleSet
	rulesForWarnings := cfg.Routing.Rules
	if input.Rules != nil {
		if err = config.ValidateRoutingRulesJSON(input.Rules); err != nil {
			writeRoutingValidationError(c, err)
			return
		}
		var rules []config.RoutingRule
		if err = json.Unmarshal(input.Rules, &rules); err != nil {
			writeRoutingValidationError(c, fmt.Errorf("routing.rules: %w", err))
			return
		}
		rulesForWarnings = rules
		draftConfig := cfg.CloneForRuntime()
		draftConfig.Routing.Rules = rules
		draft, warnings, err = routing.Compile(draftConfig)
		if err != nil {
			writeRoutingValidationError(c, err)
			return
		}
	}

	headers := make(http.Header, len(input.Headers))
	for name, value := range input.Headers {
		headers.Add(name, value)
	}
	trace, err := h.authManager.DryRunRoute(c.Request.Context(), routing.DryRunRequest{
		Model:      input.Model,
		KeyID:      keyID,
		ClientType: input.ClientType,
		Headers:    headers,
		SessionID:  input.SessionID,
		Rules:      draft,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "routing dry run failed"})
		return
	}
	labels := make(map[string]string)
	credentialIDs := make(map[string]struct{})
	for _, auth := range h.authManager.List() {
		if auth != nil {
			labels[auth.ID] = routingCredentialLabel(auth)
			credentialIDs[auth.ID] = struct{}{}
		}
	}
	for ruleIndex, rule := range rulesForWarnings {
		for targetIndex, target := range rule.RouteTo {
			if _, ok := credentialIDs[target]; !ok {
				warnings = append(warnings, fmt.Sprintf("routing.rules[%d].route-to[%d]: unknown credential %q", ruleIndex, targetIndex, target))
			}
		}
	}
	if trace != nil {
		for index := range trace.Candidates {
			trace.Candidates[index].Label = labels[trace.Candidates[index].ID]
		}
	}
	c.JSON(http.StatusOK, gin.H{"trace": trace, "warnings": warnings})
}

func (h *Handler) routingConfigSnapshot() *config.Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return &config.Config{}
	}
	return h.cfg.CloneForRuntime()
}

func routingAPIKeyID(cfg *config.Config, label string) (id string, found, ambiguous bool) {
	if label == "" {
		return "", true, false
	}
	if cfg == nil {
		return "", false, false
	}
	for _, entry := range cfg.APIKeys {
		if !strings.EqualFold(entry.Label, label) {
			continue
		}
		candidate := config.APIKeyID(entry.Key)
		if found && candidate != id {
			return "", true, true
		}
		id = candidate
		found = true
	}
	return id, found, false
}

func routingCredentialLabel(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if label := strings.TrimSpace(authAttribute(auth, "label")); label != "" {
		return label
	}
	if label := strings.TrimSpace(auth.Label); label != "" {
		return label
	}
	if provider := strings.TrimSpace(auth.Provider); provider != "" {
		return provider
	}
	return auth.ID
}

func routingCredentialPriority(auth *coreauth.Auth) int {
	if auth == nil {
		return 0
	}
	if raw := strings.TrimSpace(authAttribute(auth, "priority")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil {
			return value
		}
	}
	if auth.Metadata != nil {
		if raw, ok := auth.Metadata["priority"]; ok {
			switch value := raw.(type) {
			case int:
				return value
			case int32:
				return int(value)
			case int64:
				return int(value)
			case float64:
				return int(value)
			case string:
				if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
					return parsed
				}
			}
		}
	}
	return 0
}

func writeRoutingValidationError(c *gin.Context, err error) {
	message := "invalid routing rules"
	path := "routing.rules"
	if err != nil {
		if parsedPath, reason, ok := strings.Cut(err.Error(), ": "); ok {
			path, message = parsedPath, reason
		} else {
			message = err.Error()
		}
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": message, "path": path})
}
