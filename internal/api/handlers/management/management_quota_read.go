package management

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// authorizeReadOnlyQuota validates the complete request before credential resolution.
// Cache eligibility alone is insufficient: some cacheable probes spend credits or
// accept caller-selected RPC payloads. Caller-selected proxies and redirects are forbidden.
func authorizeReadOnlyQuota(body *apiCallRequest, auth *coreauth.Auth, target *url.URL) bool {
	if auth == nil || target == nil || target.User != nil || target.Scheme != "https" || body.ProxyURL != "" {
		return false
	}
	method := strings.ToUpper(strings.TrimSpace(body.Method))
	allowed := method == http.MethodGet && strings.TrimSpace(body.Data) == "" && classifyQuotaRequest(auth, method, target, "") == quotaRequestCacheable
	if strings.EqualFold(auth.Provider, "kimi") && method == http.MethodGet && body.Data == "" && exactQuotaURL(target,
		"https://api.kimi.ai/coding/v1/usages", "https://api.kimi.com/coding/v1/me", "https://api.kimi.ai/coding/v1/me") {
		allowed = true
	}
	if strings.EqualFold(auth.Provider, "codex") && method == http.MethodGet && body.Data == "" && target.Host == "chatgpt.com" && target.EscapedPath() == "/backend-api/subscriptions" && target.Fragment == "" {
		q, err := url.ParseQuery(target.RawQuery)
		accountID, _ := auth.Metadata["account_id"].(string)
		if err == nil && accountID != "" && len(q) == 1 && len(q["account_id"]) == 1 && q.Get("account_id") == accountID {
			allowed = true
		}
	}
	if strings.EqualFold(auth.Provider, "xai") && method == http.MethodGet && body.Data == "" && exactQuotaURL(target, "https://cli-chat-proxy.grok.com/v1/settings", "https://api.x.ai/v1/me") {
		allowed = true
	}
	if strings.EqualFold(auth.Provider, "antigravity") && method == http.MethodPost && isAntigravityQuotaURL(target) {
		var payload map[string]string
		if json.Unmarshal([]byte(body.Data), &payload) == nil && len(payload) == 1 && payload["project"] != "" && payload["project"] == authProjectID(auth) {
			allowed = true
			canonical, _ := json.Marshal(payload)
			body.Data = string(canonical)
		}
	}
	if strings.EqualFold(auth.Provider, "devin") && method == http.MethodPost && exactQuotaURL(target, "https://server.codeium.com/exa.seat_management_pb.SeatManagementService/GetUserStatus") {
		var payload struct {
			Metadata map[string]string `json:"metadata"`
		}
		expected := map[string]string{"ideName": "chisel", "ideVersion": "3000.10.21", "apiKey": "$TOKEN$", "locale": "en", "os": "darwin", "extensionVersion": "3000.10.21", "clientName": "chisel"}
		var raw map[string]json.RawMessage
		if json.Unmarshal([]byte(body.Data), &raw) == nil && len(raw) == 1 && json.Unmarshal([]byte(body.Data), &payload) == nil && len(payload.Metadata) == len(expected) {
			allowed = true
			for key, value := range expected {
				if payload.Metadata[key] != value {
					allowed = false
				}
			}
			if allowed {
				canonical, _ := json.Marshal(map[string]any{"metadata": expected})
				body.Data = string(canonical)
			}
		}
	}
	if !allowed {
		return false
	}
	for key, value := range body.Header {
		if strings.ContainsAny(value, "\r\n") {
			return false
		}
		switch strings.ToLower(key) {
		case "authorization":
			if value != "Bearer $TOKEN$" {
				return false
			}
		case "accept":
			if value != "application/json" && value != "*/*" {
				return false
			}
		case "content-type":
			if value != "application/json" {
				return false
			}
		case "anthropic-beta":
			if value != "oauth-2025-04-20" {
				return false
			}
		case "connect-protocol-version":
			if value != "1" {
				return false
			}
		case "x-xai-token-auth":
			if value != "xai-grok-cli" {
				return false
			}
		case "chatgpt-account-id":
			accountID, _ := auth.Metadata["account_id"].(string)
			if accountID == "" || value != accountID {
				return false
			}
		case "user-agent", "x-grok-client-version":
			// These headers cannot change destination or operation, but cannot resolve secrets.
			if len(value) > 256 || strings.Contains(value, "$TOKEN$") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

type readOnlyQuotaContextKey struct{}

func isReadOnlyManagement(c *gin.Context) bool {
	return c.GetString(managementScopeContextKey) == config.ManagementScopeReadOnly
}

// No upstream headers are needed by quota cards. In particular, Set-Cookie and
// authentication response headers must never become management credentials.
func sanitizeReadOnlyQuota(outcome *quotaCallOutcome, auth *coreauth.Auth, resolvedTokens ...string) {
	outcome.response.Header = nil
	var payload any
	if json.Unmarshal([]byte(outcome.response.Body), &payload) != nil {
		outcome.response.Body = "{}"
		return
	}
	secrets := append([]string{}, resolvedTokens...)
	var collect func(any, bool)
	collect = func(value any, sensitive bool) {
		switch value := value.(type) {
		case map[string]any:
			for key, child := range value {
				name := strings.ToLower(key)
				collect(child, sensitive || strings.Contains(name, "token") || strings.Contains(name, "key") || strings.Contains(name, "secret") || strings.Contains(name, "password"))
			}
		case []any:
			for _, child := range value {
				collect(child, sensitive)
			}
		case string:
			if sensitive && value != "" {
				secrets = append(secrets, value)
			}
		}
	}
	if auth != nil {
		collect(auth.Metadata, false)
		for key, value := range auth.Attributes {
			collect(map[string]any{key: value}, false)
		}
	}

	payload = sanitizeQuotaJSON(payload, secrets)
	encoded, err := json.Marshal(payload)
	if err == nil {
		outcome.response.Body = string(encoded)
	} else {
		outcome.response.Body = "{}"
	}
}

func sanitizeQuotaJSON(value any, secrets []string) any {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			name := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
			if (strings.Contains(name, "token") && !isQuotaTokenCount(name, child)) || strings.Contains(name, "secret") || strings.Contains(name, "password") || name == "api_key" || name == "apikey" || name == "authorization" || name == "cookie" {
				delete(value, key)
				continue
			}
			value[key] = sanitizeQuotaJSON(child, secrets)
		}
		return value
	case []any:
		for i, child := range value {
			value[i] = sanitizeQuotaJSON(child, secrets)
		}
		return value
	case string:
		for _, secret := range secrets {
			if secret != "" {
				value = strings.ReplaceAll(value, secret, "[redacted]")
			}
		}
		return value
	default:
		return value
	}
}

// Only recognized numeric accounting fields may bypass token credential stripping.
// A numeric access_token or a string disguised as a token count remains sensitive.
func isQuotaTokenCount(name string, value any) bool {
	if _, ok := value.(float64); !ok {
		return false
	}
	switch strings.ReplaceAll(name, "_", "") {
	case "tokens", "totaltokens", "inputtokens", "outputtokens", "prompttokens", "completiontokens", "cachedtokens", "cachecreationinputtokens", "cachereadinputtokens", "reasoningtokens", "tokensused", "usedtokens", "tokensremaining", "remainingtokens", "tokenslimit", "tokenlimit", "tokensquota", "tokenquota":
		return true
	default:
		return false
	}
}
