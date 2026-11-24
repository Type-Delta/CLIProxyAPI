package cliproxy

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/antigravitycapture"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

// RefreshAntigravityReference captures the official private CLI using a CPA credential.
func RefreshAntigravityReference(ctx context.Context, cfg *config.Config, force bool) (*antigravitycapture.Profile, error) {
	if cfg == nil || cfg.Home.Enabled {
		return nil, errors.New("Antigravity reference capture requires local credential routing")
	}
	store := sdkAuth.GetTokenStore()
	if setter, ok := store.(interface{ SetBaseDir(string) }); ok {
		setter.SetBaseDir(cfg.AuthDir)
	}
	auths, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	return antigravitycapture.EnsureCurrent(ctx, antigravityReferenceCredentials(auths, cfg.ProxyURL, time.Now()), force)
}

func (s *Service) startAntigravityReferenceUpdater(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	if s == nil || s.coreManager == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(claudeReferencePollInterval)
		defer ticker.Stop()
		var retryAfter time.Time
		for {
			s.cfgMu.RLock()
			cfg := s.cfg
			s.cfgMu.RUnlock()
			now := time.Now()
			if cfg != nil && cfg.Antigravity.OAuthSafeguard && !cfg.Home.Enabled && !now.Before(retryAfter) {
				if credentials := antigravityReferenceCredentials(s.coreManager.List(), cfg.ProxyURL, now); len(credentials) > 0 {
					if _, err := antigravitycapture.EnsureCurrent(ctx, credentials, false); err != nil && ctx.Err() == nil {
						log.WithError(err).Warn("Antigravity reference update failed")
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
	return done
}

func antigravityReferenceCredentials(auths []*coreauth.Auth, defaultProxy string, now time.Time) []antigravitycapture.Credential {
	var candidates []antigravitycapture.Credential
	seen := make(map[string]struct{})
	for _, auth := range auths {
		if auth == nil || !strings.EqualFold(auth.Provider, "antigravity") || auth.AuthKind() != coreauth.AuthKindOAuth ||
			auth.Disabled || auth.Status == coreauth.StatusDisabled || !auth.HasValidAccessToken(now) {
			continue
		}
		if auth.Unavailable && (auth.NextRetryAfter.IsZero() || auth.NextRetryAfter.After(now)) {
			continue
		}
		if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && (auth.Quota.NextRecoverAt.IsZero() || auth.Quota.NextRecoverAt.After(now)) {
			continue
		}
		if base := resolveAntigravityModelBaseURL(auth); base != "" {
			address, err := url.Parse(base)
			if err != nil || !antigravitycapture.IsDirectURL(address) {
				continue
			}
		}
		token, _ := auth.Metadata["access_token"].(string)
		project, _ := auth.Metadata["project_id"].(string)
		expires, _ := auth.AccessTokenExpirationTime()
		if token = strings.TrimSpace(token); token == "" || strings.TrimSpace(project) == "" || !expires.After(now.Add(2*time.Minute)) {
			continue
		}
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		proxy := strings.TrimSpace(auth.ProxyURL)
		if proxy == "" {
			proxy = defaultProxy
		}
		candidates = append(candidates, antigravitycapture.Credential{
			AccessToken: token, ExpiresAt: expires, Project: strings.TrimSpace(project), ProxyURL: proxy,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].ExpiresAt.After(candidates[j].ExpiresAt) })
	return candidates
}
