package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudecapture"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const claudeReferencePollInterval = time.Minute
const claudeReferenceFailureRetryInterval = 5 * time.Minute

// RefreshClaudeReference updates the private Claude Code reference using a CPA
// OAuth credential. It never consults the user's Claude Code installation.
func RefreshClaudeReference(ctx context.Context, cfg *config.Config, force bool) (*claudecapture.Profile, error) {
	if cfg == nil {
		return nil, errors.New("Claude reference update requires a CPA config")
	}
	store := sdkAuth.GetTokenStore()
	if setter, ok := store.(interface{ SetBaseDir(string) }); ok {
		setter.SetBaseDir(cfg.AuthDir)
	}
	auths, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	tokens := claudeReferenceTokens(auths, time.Now())
	if len(tokens) == 0 {
		return nil, errors.New("Claude reference update requires a current CPA Claude OAuth credential; run --claude-login")
	}
	return updateClaudeReference(ctx, tokens, force, func(updateCtx context.Context, token string, updateForce bool) (*claudecapture.Profile, error) {
		if updateForce {
			return claudecapture.UpdateNow(updateCtx, token)
		}
		return claudecapture.EnsureCurrent(updateCtx, token)
	})
}

func (s *Service) startClaudeReferenceUpdater(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(claudeReferencePollInterval)
		defer ticker.Stop()
		var retryAfter time.Time
		for {
			s.cfgMu.RLock()
			enabled := s.cfg != nil && s.cfg.ClaudeHeaderDefaults.OAuthSafeguard && !s.cfg.Home.Enabled
			s.cfgMu.RUnlock()
			if enabled && !time.Now().Before(retryAfter) {
				if tokens := claudeReferenceTokens(s.coreManager.List(), time.Now()); len(tokens) > 0 {
					_, err := updateClaudeReference(ctx, tokens, false, func(updateCtx context.Context, token string, _ bool) (*claudecapture.Profile, error) {
						return claudecapture.EnsureCurrent(updateCtx, token)
					})
					if err != nil && ctx.Err() == nil {
						log.WithError(err).Warn("Claude Code reference update failed")
						retryAfter = time.Now().Add(claudeReferenceFailureRetryInterval)
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func claudeReferenceToken(auths []*coreauth.Auth, now time.Time) string {
	tokens := claudeReferenceTokens(auths, now)
	if len(tokens) == 0 {
		return ""
	}
	return tokens[0]
}

func claudeReferenceTokens(auths []*coreauth.Auth, now time.Time) []string {
	auths = append([]*coreauth.Auth(nil), auths...)
	sort.Slice(auths, func(i, j int) bool {
		if auths[i] == nil {
			return false
		}
		if auths[j] == nil {
			return true
		}
		if !auths[i].LastRefreshedAt.Equal(auths[j].LastRefreshedAt) {
			return auths[i].LastRefreshedAt.After(auths[j].LastRefreshedAt)
		}
		return auths[i].ID < auths[j].ID
	})
	tokens := make([]string, 0, len(auths))
	seen := make(map[string]struct{}, len(auths))
	for _, auth := range auths {
		if auth == nil || !strings.EqualFold(auth.Provider, "claude") || auth.Disabled || auth.Status == coreauth.StatusDisabled || auth.AuthKind() != coreauth.AuthKindOAuth || !auth.HasValidAccessToken(now) {
			continue
		}
		if auth.Unavailable && (auth.NextRetryAfter.IsZero() || auth.NextRetryAfter.After(now)) {
			continue
		}
		if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && (auth.Quota.NextRecoverAt.IsZero() || auth.Quota.NextRecoverAt.After(now)) {
			continue
		}
		if auth.Attributes[coreauth.AttributeAPIKey] != "" || !isDirectAnthropicBase(auth.Attributes["base_url"]) {
			continue
		}
		if token, ok := auth.Metadata["access_token"].(string); ok && strings.HasPrefix(token, "sk-ant-oat") {
			if _, exists := seen[token]; !exists {
				seen[token] = struct{}{}
				tokens = append(tokens, token)
			}
		}
	}
	return tokens
}

func updateClaudeReference(ctx context.Context, tokens []string, force bool, update func(context.Context, string, bool) (*claudecapture.Profile, error)) (*claudecapture.Profile, error) {
	if len(tokens) == 0 {
		return nil, errors.New("Claude reference update requires a current Claude OAuth credential")
	}
	if update == nil {
		return nil, errors.New("Claude reference update function is unavailable")
	}
	errs := make([]error, 0, len(tokens))
	for _, token := range tokens {
		profile, err := update(ctx, token, force)
		if err == nil {
			return profile, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		errs = append(errs, err)
	}
	return nil, fmt.Errorf("all eligible Claude OAuth reference credentials failed: %w", errors.Join(errs...))
}

func isDirectAnthropicBase(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "api.anthropic.com") || (parsed.Port() != "" && parsed.Port() != "443") {
		return false
	}
	return (parsed.Path == "" || parsed.Path == "/") && parsed.RawQuery == "" && parsed.Fragment == ""
}
