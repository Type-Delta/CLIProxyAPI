package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// RequestKiroToken starts the IAM SSO browser flow used by Kiro credentials.
func (h *Handler) RequestKiroToken(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	ctx := PopulateAuthContext(context.Background(), c)
	state, err := misc.GenerateRandomState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate oauth state"})
		return
	}
	region := strings.TrimSpace(c.Query("region"))
	if region == "" {
		region = kiroauth.DefaultRegion
	}
	startURL := strings.TrimSpace(c.Query("start_url"))
	if startURL == "" {
		startURL = strings.TrimSpace(c.Query("startUrl"))
	}
	if startURL == "" {
		startURL = kiroauth.DefaultStartURL
	}
	// Kiro only accepts its fixed loopback redirect URI. The browser will show
	// the resulting callback URL, which CPAMC submits through the shared route.
	redirectURI := kiroauth.DefaultRedirectURI
	authSvc := kiroauth.NewAuth(h.cfg)
	authSvc.SetRegion(region)
	session, authURL, err := authSvc.StartIAMLogin(ctx, startURL, redirectURI, state, nil)
	if err != nil {
		log.WithError(err).Error("failed to start Kiro OAuth")
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to generate authorization url"})
		return
	}
	RegisterOAuthSession(state, kiroauth.Provider)
	go h.completeKiroOAuth(ctx, authSvc, session)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) completeKiroOAuth(ctx context.Context, authSvc *kiroauth.Auth, session *kiroauth.LoginSession) {
	if h == nil || h.cfg == nil || authSvc == nil || session == nil {
		return
	}
	state := session.State
	waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-%s-%s.oauth", kiroauth.Provider, state))
	deadline := time.Now().Add(10 * time.Minute)
	for {
		if !IsOAuthSessionPending(state, kiroauth.Provider) {
			return
		}
		if time.Now().After(deadline) {
			SetOAuthSessionError(state, "Timeout waiting for OAuth callback")
			return
		}
		data, errRead := os.ReadFile(waitFile)
		if errRead == nil {
			_ = os.Remove(waitFile)
			var result map[string]string
			if json.Unmarshal(data, &result) != nil || result["state"] != state {
				SetOAuthSessionError(state, "Invalid OAuth callback")
				return
			}
			if result["error"] != "" {
				SetOAuthSessionError(state, result["error"])
				return
			}
			token, errExchange := authSvc.ExchangeCode(ctx, session, strings.Split(result["code"], "#")[0])
			if errExchange != nil {
				SetOAuthSessionError(state, oauthSessionErrorWithCause("Failed to exchange authorization code for tokens", errExchange))
				return
			}
			storage := &kiroauth.TokenStorage{Type: kiroauth.Provider, ClientID: session.ClientID, ClientSecret: session.ClientSecret, Region: session.Region, AuthMethod: "oidc"}
			if errApply := kiroauth.ApplyTokenResponse(storage, token); errApply != nil {
				SetOAuthSessionError(state, errApply.Error())
				return
			}
			metadata := map[string]any{"type": kiroauth.Provider, "access_token": storage.AccessToken, "refresh_token": storage.RefreshToken, "token_type": storage.TokenType, "client_id": storage.ClientID, "client_secret": storage.ClientSecret, "region": storage.Region, "auth_method": storage.AuthMethod, "expired": storage.ExpiresAt}
			if storage.ProfileARN != "" {
				metadata["profile_arn"] = storage.ProfileARN
			}
			fileName := fmt.Sprintf("kiro-%d.json", time.Now().UnixMilli())
			attributes := map[string]string{"auth_kind": "oauth", "region": storage.Region}
			if storage.ProfileARN != "" {
				attributes["profile_arn"] = storage.ProfileARN
			}
			record := &coreauth.Auth{ID: fileName, Provider: kiroauth.Provider, FileName: fileName, Label: "Kiro", Storage: storage, Metadata: metadata, Attributes: attributes}
			if errGuard := guardOAuthSessionPendingForSave(state, kiroauth.Provider); errGuard != nil {
				return
			}
			if _, errSave := h.saveTokenRecord(ctx, record); errSave != nil {
				SetOAuthSessionError(state, oauthSessionErrorWithCause("Failed to save authentication tokens", errSave))
				return
			}
			CompleteOAuthSession(state)
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}
