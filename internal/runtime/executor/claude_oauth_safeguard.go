package executor

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecapture"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type claudeOAuthSafeguardError struct {
	statusErr
}

func (claudeOAuthSafeguardError) IsRequestScoped() bool { return true }

func newClaudeOAuthSafeguardError(message string) error {
	return claudeOAuthSafeguardError{statusErr{code: http.StatusForbidden, msg: message}}
}

type claudeOAuthSafeguard struct {
	profile *claudecapture.Profile
}

// loadClaudeOAuthSafeguard runs only for direct Anthropic requests using real
// Claude OAuth credentials. The captured software tuple replaces the fallback
// header baseline for this request, so native client detection remains current.
func (e *ClaudeExecutor) loadClaudeOAuthSafeguard(baseURL, apiKey string) (*config.Config, *claudeOAuthSafeguard, error) {
	if e.cfg == nil || !e.cfg.ClaudeHeaderDefaults.OAuthSafeguard ||
		!isClaudeOAuthToken(apiKey) || !isAnthropicUpstreamBase(baseURL) {
		return e.cfg, nil, nil
	}
	load := e.oauthSafeguardLoader
	if load == nil {
		load = claudecapture.LoadCurrent
	}
	profile, errLoad := load()
	if errLoad != nil {
		return nil, nil, newClaudeOAuthSafeguardError("Claude OAuth safeguard requires a current Claude Code capture: " + errLoad.Error())
	}
	if profile == nil {
		return nil, nil, newClaudeOAuthSafeguardError("Claude OAuth safeguard requires a current Claude Code capture")
	}
	profileHeaders := make(http.Header, len(profile.Headers))
	for name, value := range profile.Headers {
		profileHeaders.Set(name, value)
	}
	for _, name := range []string{
		"User-Agent", "X-Stainless-Package-Version", "X-Stainless-Runtime-Version",
		"X-Stainless-OS", "X-Stainless-Arch",
	} {
		if profileHeaders.Get(name) == "" {
			return nil, nil, newClaudeOAuthSafeguardError("Claude OAuth safeguard capture lacks " + name)
		}
	}
	effective := *e.cfg
	effective.ClaudeHeaderDefaults.UserAgent = profileHeaders.Get("User-Agent")
	effective.ClaudeHeaderDefaults.PackageVersion = profileHeaders.Get("X-Stainless-Package-Version")
	effective.ClaudeHeaderDefaults.RuntimeVersion = profileHeaders.Get("X-Stainless-Runtime-Version")
	effective.ClaudeHeaderDefaults.OS = profileHeaders.Get("X-Stainless-OS")
	effective.ClaudeHeaderDefaults.Arch = profileHeaders.Get("X-Stainless-Arch")
	return &effective, &claudeOAuthSafeguard{profile: profile}, nil
}

func (s *claudeOAuthSafeguard) checkIncoming(source sdktranslator.Format, headers http.Header, detection helps.ClaudeCodeRequestDetection) error {
	if s == nil {
		return nil
	}
	if source != sdktranslator.FormatClaude {
		return newClaudeOAuthSafeguardError("Claude OAuth safeguard requires a native Claude Messages client")
	}
	if !detection.Confirmed || !detection.NativeClient {
		return newClaudeOAuthSafeguardError("Claude OAuth safeguard requires a matching Claude Code request profile")
	}
	for name, want := range s.profile.Headers {
		if name == "Anthropic-Beta" {
			continue
		}
		if headers.Get(name) != want {
			return newClaudeOAuthSafeguardError("Claude OAuth safeguard rejected client header " + name)
		}
	}
	return nil
}

func (s *claudeOAuthSafeguard) checkOutbound(req *http.Request, body []byte, countTokens, helperProfile bool) error {
	if s == nil {
		return nil
	}
	if req == nil || req.URL == nil || !isAnthropicUpstreamURL(req.URL) {
		return newClaudeOAuthSafeguardError("Claude OAuth safeguard rejected the upstream destination")
	}
	if got := strings.TrimSpace(req.Header.Get("Authorization")); !strings.HasPrefix(got, "Bearer ") {
		return newClaudeOAuthSafeguardError("Claude OAuth safeguard requires OAuth bearer authentication")
	}
	if !helperProfile && !claudeRequestedBetas(req.Header.Get("Anthropic-Beta"), nil)[claudeCodeBeta] {
		return newClaudeOAuthSafeguardError("Claude OAuth safeguard requires the Claude Code beta")
	}
	endpoint := "messages"
	if countTokens {
		endpoint = "count_tokens"
	}
	if errMatch := s.profile.CheckRequest(req.Header, body, endpoint); errMatch != nil {
		return newClaudeOAuthSafeguardError(fmt.Sprintf("Claude OAuth safeguard rejected outbound request: %v", errMatch))
	}
	if errShape := s.checkBodyShape(body, countTokens); errShape != nil {
		return errShape
	}
	if !countTokens {
		userID := gjson.GetBytes(body, "metadata.user_id").String()
		if !helps.IsValidUserID(userID) || req.Header.Get("X-Claude-Code-Session-Id") != gjson.Get(userID, "session_id").String() {
			return newClaudeOAuthSafeguardError("Claude OAuth safeguard rejected outbound session identity")
		}
	}
	return nil
}

func (s *claudeOAuthSafeguard) checkBodyShape(body []byte, countTokens bool) error {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() ||
		gjson.GetBytes(body, "model").Type != gjson.String || !gjson.GetBytes(body, "messages").IsArray() {
		return newClaudeOAuthSafeguardError("Claude OAuth safeguard rejected outbound body shape")
	}
	if !countTokens && gjson.GetBytes(body, "metadata.user_id").Type != gjson.String {
		return newClaudeOAuthSafeguardError("Claude OAuth safeguard requires native session metadata")
	}
	return nil
}
