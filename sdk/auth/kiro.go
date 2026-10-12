package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const kiroCallbackPort = 54547

// KiroAuthenticator implements IAM SSO browser OAuth for Kiro.
type KiroAuthenticator struct{ CallbackPort int }

func NewKiroAuthenticator() Authenticator             { return &KiroAuthenticator{CallbackPort: kiroCallbackPort} }
func (KiroAuthenticator) Provider() string            { return kiroauth.Provider }
func (KiroAuthenticator) RefreshLead() *time.Duration { lead := 5 * time.Minute; return &lead }

func (a *KiroAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}
	callback := kiroauth.PortalRedirectURI
	state, err := misc.GenerateRandomState()
	if err != nil {
		return nil, fmt.Errorf("kiro state generation failed: %w", err)
	}
	pkce, err := kiroauth.GeneratePKCECodes()
	if err != nil {
		return nil, err
	}
	authSvc := kiroauth.NewAuth(cfg)
	authURL := kiroauth.BuildPortalURL(state, pkce.CodeChallenge, callback)
	if !opts.NoBrowser {
		if browser.IsAvailable() {
			if errOpen := browser.OpenURL(authURL); errOpen != nil {
				log.WithError(errOpen).Warn("failed to open browser for Kiro authentication")
				fmt.Printf("Visit the following URL to continue Kiro authentication:\n%s\n", authURL)
			}
		} else {
			fmt.Printf("Visit the following URL to continue Kiro authentication:\n%s\n", authURL)
		}
	} else {
		fmt.Printf("Visit the following URL to continue Kiro authentication:\n%s\n", authURL)
	}
	if opts.Prompt == nil {
		return nil, fmt.Errorf("kiro OAuth requires a callback prompt")
	}
	inputCh, errCh := misc.AsyncPrompt(opts.Prompt, "Paste the Kiro callback URL (or query string) after login: ")
	var input string
	select {
	case input = <-inputCh:
	case err = <-errCh:
		return nil, fmt.Errorf("kiro OAuth callback: %w", err)
	}
	portalCallback, err := kiroauth.ParsePortalCallback(input)
	if err != nil || portalCallback == nil {
		return nil, fmt.Errorf("kiro OAuth callback: invalid portal callback")
	}
	if portalCallback.Error != "" {
		return nil, fmt.Errorf("kiro OAuth denied: %s", portalCallback.Error)
	}
	if strings.TrimSpace(portalCallback.State) != strings.TrimSpace(state) {
		return nil, fmt.Errorf("kiro OAuth state mismatch")
	}
	loginOption := strings.ToLower(strings.TrimSpace(portalCallback.LoginOption))
	if loginOption != "awsidc" {
		return nil, fmt.Errorf("kiro OAuth login option %q is not supported yet; choose Your organization", loginOption)
	}
	region := portalCallback.IDCRegion
	if region == "" {
		region = kiroauth.DefaultRegion
	}
	deviceSession, err := authSvc.StartDeviceAuthorization(ctx, portalCallback.IssuerURL, region)
	if err != nil {
		return nil, err
	}
	fmt.Printf("Kiro organization authorization URL:\n%s\n", deviceSession.VerificationURL)
	if deviceSession.UserCode != "" {
		fmt.Printf("Kiro organization device code: %s\n", deviceSession.UserCode)
	}
	token, err := authSvc.WaitDeviceAuthorization(ctx, deviceSession)
	if err != nil {
		return nil, err
	}
	storage := &kiroauth.TokenStorage{
		Type:         kiroauth.Provider,
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		TokenType:    token.TokenType,
		ClientID:     deviceSession.ClientID,
		ClientSecret: deviceSession.ClientSecret,
		Region:       deviceSession.Region,
		AuthMethod:   "oidc",
	}
	if err := kiroauth.ApplyTokenResponse(storage, token); err != nil {
		return nil, err
	}
	fileName := fmt.Sprintf("kiro-%d.json", time.Now().UnixMilli())
	metadata := map[string]any{
		"type":          kiroauth.Provider,
		"access_token":  storage.AccessToken,
		"refresh_token": storage.RefreshToken,
		"token_type":    storage.TokenType,
		"client_id":     storage.ClientID,
		"client_secret": storage.ClientSecret,
		"region":        storage.Region,
		"auth_method":   storage.AuthMethod,
		"expired":       storage.ExpiresAt,
	}
	if storage.ProfileARN != "" {
		metadata["profile_arn"] = storage.ProfileARN
	}
	attributes := map[string]string{"auth_kind": "oauth", "region": storage.Region}
	if storage.ProfileARN != "" {
		attributes["profile_arn"] = storage.ProfileARN
	}
	return &coreauth.Auth{ID: fileName, Provider: kiroauth.Provider, FileName: fileName, Label: "Kiro", Storage: storage, Metadata: metadata, Attributes: attributes}, nil
}
