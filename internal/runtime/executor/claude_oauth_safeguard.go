package executor

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecapture"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

var claudeOAuthSafeguardUserAgentPattern = regexp.MustCompile(`(?i)^claude-cli/[0-9]+\.[0-9]+\.[0-9]+\s+\(external,\s*[^,)]+(?:,\s*agent-sdk/[0-9]+\.[0-9]+\.[0-9]+)?\)$`)

type claudeOAuthSafeguardError struct {
	statusErr
}

func (claudeOAuthSafeguardError) IsRequestScoped() bool { return true }

func newClaudeOAuthSafeguardError(message string) error {
	return newClaudeOAuthSafeguardErrorWithReason(message, message)
}

func newClaudeOAuthSafeguardErrorWithReason(message, reason string) error {
	log.WithField("reason", reason).Warn("Claude OAuth safeguard rejected request")
	return claudeOAuthSafeguardError{statusErr{code: http.StatusForbidden, msg: message}}
}

type claudeOAuthSafeguard struct {
	profile *claudecapture.Profile
}

// loadClaudeOAuthSafeguard runs only for direct Anthropic requests using real
// Claude OAuth credentials. Captured headers validate the wire profile without
// changing the configured Claude header defaults.
func (e *ClaudeExecutor) loadClaudeOAuthSafeguard(baseURL, apiKey string) (*config.Config, *claudeOAuthSafeguard, error) {
	if e.cfg == nil || !e.cfg.ClaudeHeaderDefaults.OAuthSafeguard ||
		!isClaudeOAuthToken(apiKey) || !isAnthropicUpstreamBase(baseURL) {
		return claudeOAuthDetectionConfig(e.cfg), nil, nil
	}
	load := e.oauthSafeguardLoader
	if load == nil {
		load = claudecapture.LoadCurrent
	}
	profile, errLoad := load()
	if errLoad != nil {
		return nil, nil, newClaudeOAuthSafeguardErrorWithReason(
			"Claude OAuth safeguard requires a current Claude Code capture: "+errLoad.Error(),
			"current Claude Code capture unavailable",
		)
	}
	if profile == nil {
		return nil, nil, newClaudeOAuthSafeguardError("Claude OAuth safeguard requires a current Claude Code capture")
	}
	if profile.Headers["User-Agent"] == "" {
		return nil, nil, newClaudeOAuthSafeguardError("Claude OAuth safeguard capture lacks User-Agent")
	}
	return e.cfg, &claudeOAuthSafeguard{profile: profile}, nil
}

// claudeOAuthDetectionConfig keeps the full runtime configuration available to
// header reconstruction while preventing an enabled safeguard from relaxing
// client detection on requests that are outside its OAuth scope.
func claudeOAuthDetectionConfig(cfg *config.Config) *config.Config {
	if cfg == nil || !cfg.ClaudeHeaderDefaults.OAuthSafeguard {
		return cfg
	}
	copyCfg := *cfg
	copyCfg.ClaudeHeaderDefaults.OAuthSafeguard = false
	return &copyCfg
}

func (s *claudeOAuthSafeguard) checkIncoming(source sdktranslator.Format, headers http.Header, detection helps.ClaudeCodeRequestDetection) error {
	if s == nil {
		return nil
	}
	userAgent := strings.TrimSpace(headers.Get("User-Agent"))
	if !claudeOAuthSafeguardUserAgentPattern.MatchString(userAgent) {
		return newClaudeOAuthSafeguardErrorWithReason(
			fmt.Sprintf("Claude OAuth safeguard flagged this request because Claude expects User-Agent to be 'claude-cli/X.X.X' but received '%s'.", userAgent),
			"client User-Agent does not match claude-cli/X.X.X",
		)
	}
	if source != sdktranslator.FormatClaude {
		return newClaudeOAuthSafeguardErrorWithReason(
			fmt.Sprintf("Claude OAuth safeguard requires a native Claude Messages client; expected a claude-cli/X.X.X User-Agent, received %q", userAgent),
			"request source is not Claude Messages",
		)
	}
	if !detection.NativeClient {
		return newClaudeOAuthSafeguardErrorWithReason(
			fmt.Sprintf("Claude OAuth safeguard requires a supported Claude Code client entrypoint (cli, sdk, sdk-cli, sdk-ts, sdk-py, claude-vscode, or claude-desktop); received %q.", detection.Entrypoint),
			"client entrypoint is not supported for native Claude Code requests",
		)
	}
	if !detection.Confirmed {
		message, reason := "Claude OAuth safeguard requires a matching Claude Code request profile.", "native Claude Code request signals were not confirmed"
		switch {
		case !detection.XAppCLI:
			xApp := strings.TrimSpace(headers.Get("X-App"))
			message, reason = fmt.Sprintf("Claude OAuth safeguard expects X-App to be 'cli'; received %q.", xApp), "client X-App is not cli"
		case !detection.BetasPresent && !detection.HelperProfile:
			message, reason = "Claude OAuth safeguard expects the Claude Code beta or a supported native helper request.", "client lacks Claude Code beta and supported helper profile"
		case !detection.MetadataUserID:
			message, reason = "Claude OAuth safeguard expects valid Claude Code session metadata.", "client session metadata is missing or invalid"
		}
		return newClaudeOAuthSafeguardErrorWithReason(
			message,
			reason,
		)
	}
	for name, want := range s.profile.Headers {
		if name == "Anthropic-Beta" || name == "User-Agent" ||
			name == "X-Stainless-Package-Version" || name == "X-Stainless-Runtime-Version" ||
			name == "X-Stainless-OS" || name == "X-Stainless-Arch" {
			continue
		}
		if headers.Get(name) != want {
			return newClaudeOAuthSafeguardErrorWithReason(
				"Claude OAuth safeguard rejected client header "+name,
				"client header does not match captured reference: "+name,
			)
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
