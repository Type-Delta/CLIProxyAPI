// Package kiro implements Kiro (Amazon Q Developer) OAuth and token refresh.
package kiro

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	Provider                  = "kiro"
	DefaultRegion             = "us-east-1"
	DefaultStartURL           = "https://view.awsapps.com/start"
	DefaultRedirectURI        = "http://127.0.0.1/oauth/callback"
	DesktopRefreshURLTemplate = "https://prod.%s.auth.desktop.kiro.dev/refreshToken"
	OIDCBaseURLTemplate       = "https://oidc.%s.amazonaws.com"
	RuntimeURLTemplate        = "https://runtime.%s.kiro.dev/generateAssistantResponse"
	LegacyRuntimeURLTemplate  = "https://q.%s.amazonaws.com/generateAssistantResponse"
	PortalURL                 = "https://app.kiro.dev/signin"
	PortalRedirectURI         = "http://localhost:3128"
)

var oidcScopes = []string{
	"codewhisperer:completions",
	"codewhisperer:analysis",
	"codewhisperer:conversations",
	"codewhisperer:transformations",
	"codewhisperer:taskassist",
}

// LoginSession contains the state required to exchange an IAM SSO callback.
type LoginSession struct {
	ClientID     string
	ClientSecret string
	CodeVerifier string
	State        string
	Region       string
	StartURL     string
	RedirectURI  string
	ExpiresAt    time.Time
}

// TokenResponse is the camelCase token response returned by Kiro OIDC/Desktop.
type TokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	TokenType    string `json:"tokenType"`
	ExpiresIn    int64  `json:"expiresIn"`
	ProfileARN   string `json:"profileArn"`
}

// PortalCallback is the first callback emitted by Kiro's sign-in portal.
// Identity Center callbacks carry issuer_url and idc_region; the portal then
// expects the client to complete the AWS OIDC device flow.
type PortalCallback struct {
	LoginOption string
	IssuerURL   string
	IDCRegion   string
	State       string
	Error       string
}

// DeviceSession contains the private client registration and device code used
// after the Kiro portal selects an identity provider.
type DeviceSession struct {
	ClientID        string
	ClientSecret    string
	DeviceCode      string
	UserCode        string
	VerificationURL string
	Interval        time.Duration
	ExpiresAt       time.Time
	Region          string
	StartURL        string
}

// Auth provides Kiro control-plane operations.
type Auth struct {
	client *http.Client
	region string
}

// KiroAuth is the descriptive alias used by provider integrations.
type KiroAuth = Auth

// NewAuth creates a Kiro auth service using configured proxy settings.
func NewAuth(cfg *config.Config) *Auth {
	if cfg == nil {
		return &Auth{client: &http.Client{}, region: DefaultRegion}
	}
	return &Auth{
		client: util.SetProxy(&cfg.SDKConfig, &http.Client{}),
		region: DefaultRegion,
	}
}

// NewKiroAuth is an explicit provider-named constructor alias.
func NewKiroAuth(cfg *config.Config) *Auth { return NewAuth(cfg) }

// RefreshToken is a convenience wrapper for one-shot refresh operations.
func RefreshToken(ctx context.Context, cfg *config.Config, storage *TokenStorage) (*TokenResponse, error) {
	return NewAuth(cfg).RefreshToken(ctx, storage)
}

// NewAuthWithHTTPClient is useful for deterministic tests and management adapters.
func NewAuthWithHTTPClient(client *http.Client, region string) *Auth {
	if client == nil {
		client = &http.Client{}
	}
	return &Auth{client: client, region: normalizeRegion(region)}
}

// SetRegion selects the OIDC region for subsequent operations.
func (a *Auth) SetRegion(region string) {
	if a != nil {
		a.region = normalizeRegion(region)
	}
}

func normalizeRegion(region string) string {
	if region = strings.TrimSpace(region); region != "" {
		return region
	}
	return DefaultRegion
}

func (a *Auth) httpClient() *http.Client {
	if a == nil || a.client == nil {
		return http.DefaultClient
	}
	return a.client
}

func (a *Auth) regionFor(region string) string {
	if strings.TrimSpace(region) != "" {
		return normalizeRegion(region)
	}
	if a != nil {
		return normalizeRegion(a.region)
	}
	return DefaultRegion
}

// BuildAuthorizationURL creates an IAM SSO authorization URL after the caller
// has supplied the registered client and session state.
func BuildAuthorizationURL(region string, session *LoginSession) string {
	if session == nil {
		return ""
	}
	base := fmt.Sprintf(OIDCBaseURLTemplate, normalizeRegion(region))
	values := url.Values{}
	values.Set("response_type", "code")
	values.Set("client_id", session.ClientID)
	values.Set("redirect_uri", session.RedirectURI)
	values.Set("scopes", strings.Join(oidcScopes, ","))
	values.Set("state", session.State)
	values.Set("code_challenge", session.CodeChallenge())
	values.Set("code_challenge_method", "S256")
	return base + "/authorize?" + values.Encode()
}

// BuildPortalURL creates the selector page used by the official Kiro CLI.
func BuildPortalURL(state, codeChallenge, redirectURI string) string {
	values := url.Values{}
	values.Set("state", strings.TrimSpace(state))
	values.Set("code_challenge", strings.TrimSpace(codeChallenge))
	values.Set("code_challenge_method", "S256")
	values.Set("redirect_uri", strings.TrimSpace(redirectURI))
	values.Set("redirect_from", "kirocli")
	return PortalURL + "?" + values.Encode()
}

// ParsePortalCallback parses the Kiro portal's first-stage callback URL.
func ParsePortalCallback(raw string) (*PortalCallback, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("parse Kiro portal callback: %w", err)
	}
	query := parsed.Query()
	result := &PortalCallback{
		LoginOption: strings.TrimSpace(query.Get("login_option")),
		IssuerURL:   strings.TrimSpace(query.Get("issuer_url")),
		IDCRegion:   strings.TrimSpace(query.Get("idc_region")),
		State:       strings.TrimSpace(query.Get("state")),
		Error:       strings.TrimSpace(query.Get("error")),
	}
	if result.State == "" {
		return nil, fmt.Errorf("Kiro portal callback is missing state")
	}
	if result.Error != "" {
		return result, nil
	}
	return result, nil
}

// CodeChallenge computes the PKCE challenge for the session verifier.
func (s *LoginSession) CodeChallenge() string {
	if s == nil {
		return ""
	}
	hash := sha256Bytes([]byte(s.CodeVerifier))
	return base64RawURL(hash)
}

// StartIAMLogin registers a public OIDC client and builds its PKCE URL.
func (a *Auth) StartIAMLogin(ctx context.Context, startURL, redirectURI, state string, pkce *PKCECodes) (*LoginSession, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	region := a.regionFor("")
	if strings.TrimSpace(startURL) == "" {
		startURL = DefaultStartURL
	}
	if strings.TrimSpace(redirectURI) == "" {
		redirectURI = DefaultRedirectURI
	}
	if strings.TrimSpace(state) == "" {
		var err error
		state, err = misc.GenerateRandomState()
		if err != nil {
			return nil, "", fmt.Errorf("generate Kiro OAuth state: %w", err)
		}
	}
	if pkce == nil {
		var err error
		pkce, err = GeneratePKCECodes()
		if err != nil {
			return nil, "", err
		}
	}
	clientID, clientSecret, err := a.registerClient(ctx, region, startURL, redirectURI, []string{"authorization_code", "refresh_token"})
	if err != nil {
		return nil, "", err
	}
	session := &LoginSession{ClientID: clientID, ClientSecret: clientSecret, CodeVerifier: pkce.CodeVerifier, State: state, Region: region, StartURL: startURL, RedirectURI: redirectURI, ExpiresAt: time.Now().Add(10 * time.Minute)}
	return session, BuildAuthorizationURL(region, session), nil
}

func (a *Auth) registerClient(ctx context.Context, region, startURL, redirectURI string, grants []string) (string, string, error) {
	payload := map[string]any{"clientName": "Kiro", "clientType": "public", "scopes": oidcScopes, "grantTypes": grants, "issuerUrl": startURL}
	if redirectURI != "" {
		payload["redirectUris"] = []string{redirectURI}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("encode Kiro OIDC client registration: %w", err)
	}
	endpoint := fmt.Sprintf(OIDCBaseURLTemplate, normalizeRegion(region)) + "/client/register"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("create Kiro OIDC registration request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return "", "", fmt.Errorf("register Kiro OIDC client: %w", err)
	}
	defer closeKiroBody(resp.Body, "OIDC registration")
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", fmt.Errorf("read Kiro OIDC registration: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", &HTTPStatusError{Code: resp.StatusCode, Message: fmt.Sprintf("Kiro OIDC client registration failed: %s", strings.TrimSpace(string(data)))}
	}
	var result struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", "", fmt.Errorf("decode Kiro OIDC registration: %w", err)
	}
	if result.ClientID == "" || result.ClientSecret == "" {
		return "", "", fmt.Errorf("Kiro OIDC registration returned empty client credentials")
	}
	return result.ClientID, result.ClientSecret, nil
}

// StartDeviceAuthorization registers the public Kiro client and starts the
// AWS IAM Identity Center device flow selected by the Kiro portal.
func (a *Auth) StartDeviceAuthorization(ctx context.Context, startURL, region string) (*DeviceSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	region = normalizeRegion(region)
	startURL = strings.TrimSpace(startURL)
	if startURL == "" {
		return nil, fmt.Errorf("Kiro Identity Center start URL is empty")
	}
	clientID, clientSecret, err := a.registerClient(ctx, region, startURL, "", []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"})
	if err != nil {
		return nil, err
	}
	payload := map[string]string{"clientId": clientID, "clientSecret": clientSecret, "startUrl": startURL}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode Kiro device authorization: %w", err)
	}
	endpoint := fmt.Sprintf(OIDCBaseURLTemplate, region) + "/device_authorization"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Kiro device authorization request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("Kiro device authorization request: %w", err)
	}
	defer closeKiroBody(resp.Body, "device authorization")
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read Kiro device authorization response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPStatusError{Code: resp.StatusCode, Message: fmt.Sprintf("Kiro device authorization failed: %s", strings.TrimSpace(string(data)))}
	}
	var result struct {
		DeviceCode              string `json:"deviceCode"`
		UserCode                string `json:"userCode"`
		VerificationURI         string `json:"verificationUri"`
		VerificationURIComplete string `json:"verificationUriComplete"`
		Interval                int64  `json:"interval"`
		ExpiresIn               int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode Kiro device authorization response: %w", err)
	}
	if result.DeviceCode == "" {
		return nil, fmt.Errorf("Kiro device authorization returned empty device code")
	}
	interval := result.Interval
	if interval <= 0 {
		interval = 5
	}
	expiresIn := result.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}
	verificationURL := result.VerificationURIComplete
	if verificationURL == "" {
		verificationURL = result.VerificationURI
	}
	return &DeviceSession{
		ClientID: clientID, ClientSecret: clientSecret, DeviceCode: result.DeviceCode,
		UserCode: result.UserCode, VerificationURL: verificationURL,
		Interval: time.Duration(interval) * time.Second, ExpiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second),
		Region: region, StartURL: startURL,
	}, nil
}

// PollDeviceAuthorization performs one device-token poll. pending is true
// while the user has not finished the browser approval.
func (a *Auth) PollDeviceAuthorization(ctx context.Context, session *DeviceSession) (token *TokenResponse, pending bool, err error) {
	if session == nil || session.DeviceCode == "" {
		return nil, false, fmt.Errorf("Kiro device session is empty")
	}
	payload := map[string]string{"clientId": session.ClientID, "clientSecret": session.ClientSecret, "grantType": "urn:ietf:params:oauth:grant-type:device_code", "deviceCode": session.DeviceCode}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, false, fmt.Errorf("encode Kiro device token request: %w", err)
	}
	endpoint := fmt.Sprintf(OIDCBaseURLTemplate, normalizeRegion(session.Region)) + "/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("create Kiro device token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("Kiro device token request: %w", err)
	}
	defer closeKiroBody(resp.Body, "device token")
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false, fmt.Errorf("read Kiro device token response: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var result TokenResponse
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, false, fmt.Errorf("decode Kiro device token response: %w", err)
		}
		if result.AccessToken == "" {
			return nil, false, fmt.Errorf("Kiro device token response has empty access token")
		}
		return &result, false, nil
	}
	var status struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(data, &status)
	switch status.Error {
	case "authorization_pending", "slow_down":
		return nil, true, nil
	case "expired_token":
		return nil, false, fmt.Errorf("Kiro device authorization expired")
	case "access_denied":
		return nil, false, fmt.Errorf("Kiro device authorization denied")
	default:
		message := strings.TrimSpace(status.Description)
		if message == "" {
			message = strings.TrimSpace(string(data))
		}
		return nil, false, &HTTPStatusError{Code: resp.StatusCode, Message: fmt.Sprintf("Kiro device token request failed: %s", message)}
	}
}

// WaitDeviceAuthorization polls until the user completes the device flow.
func (a *Auth) WaitDeviceAuthorization(ctx context.Context, session *DeviceSession) (*TokenResponse, error) {
	if session == nil {
		return nil, fmt.Errorf("Kiro device session is empty")
	}
	interval := session.Interval
	if interval <= 0 {
		interval = time.Second
	}
	for {
		if time.Now().After(session.ExpiresAt) {
			return nil, fmt.Errorf("Kiro device authorization expired")
		}
		token, pending, err := a.PollDeviceAuthorization(ctx, session)
		if err != nil {
			return nil, err
		}
		if !pending {
			return token, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// ExchangeCode exchanges an IAM SSO authorization code for tokens.
func (a *Auth) ExchangeCode(ctx context.Context, session *LoginSession, code string) (*TokenResponse, error) {
	if session == nil {
		return nil, fmt.Errorf("Kiro OAuth session is nil")
	}
	if time.Now().After(session.ExpiresAt) {
		return nil, fmt.Errorf("Kiro OAuth session expired")
	}
	payload := map[string]string{"clientId": session.ClientID, "clientSecret": session.ClientSecret, "grantType": "authorization_code", "redirectUri": session.RedirectURI, "code": code, "codeVerifier": session.CodeVerifier}
	return a.postJSONToken(ctx, fmt.Sprintf(OIDCBaseURLTemplate, session.Region)+"/token", payload)
}

// RefreshToken refreshes Desktop/social or AWS OIDC credentials.
func (a *Auth) RefreshToken(ctx context.Context, storage *TokenStorage) (*TokenResponse, error) {
	if storage == nil || strings.TrimSpace(storage.RefreshToken) == "" {
		return nil, fmt.Errorf("Kiro refresh token is empty")
	}
	region := a.regionFor(storage.Region)
	if strings.EqualFold(strings.TrimSpace(storage.AuthMethod), "oidc") || (storage.ClientID != "" && storage.ClientSecret != "") {
		payload := map[string]string{"clientId": storage.ClientID, "clientSecret": storage.ClientSecret, "grantType": "refresh_token", "refreshToken": storage.RefreshToken}
		return a.postJSONToken(ctx, fmt.Sprintf(OIDCBaseURLTemplate, region)+"/token", payload)
	}
	payload := map[string]string{"refreshToken": storage.RefreshToken}
	return a.postJSONToken(ctx, fmt.Sprintf(DesktopRefreshURLTemplate, region), payload)
}

func (a *Auth) postJSONToken(ctx context.Context, endpoint string, payload map[string]string) (*TokenResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode Kiro token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Kiro token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("Kiro token request: %w", err)
	}
	defer closeKiroBody(resp.Body, "token")
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read Kiro token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(data))
		// AWS may describe a revoked refresh token without including the
		// standard OAuth invalid_grant code. Preserve that signal so CPA marks
		// the credential terminal and stops retrying it until the user signs in.
		lower := strings.ToLower(message)
		if strings.Contains(lower, "refresh token") && (strings.Contains(lower, "revoked") || strings.Contains(lower, "revoke")) {
			message = "invalid_grant: " + message
		}
		return nil, &HTTPStatusError{Code: resp.StatusCode, Message: fmt.Sprintf("Kiro token request failed: %s", message)}
	}
	var result TokenResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode Kiro token response: %w", err)
	}
	if strings.TrimSpace(result.AccessToken) == "" {
		return nil, fmt.Errorf("Kiro token response has empty access token")
	}
	return &result, nil
}

// ApplyTokenResponse updates storage after an exchange/refresh.
func ApplyTokenResponse(storage *TokenStorage, response *TokenResponse) error {
	if storage == nil || response == nil {
		return fmt.Errorf("Kiro token storage or response is nil")
	}
	if response.AccessToken == "" {
		return fmt.Errorf("Kiro token response has empty access token")
	}
	storage.AccessToken = response.AccessToken
	if response.RefreshToken != "" {
		storage.RefreshToken = response.RefreshToken
	}
	if response.TokenType != "" {
		storage.TokenType = response.TokenType
	}
	if response.ProfileARN != "" {
		storage.ProfileARN = response.ProfileARN
	}
	if response.ExpiresIn > 0 {
		storage.ExpiresAt = time.Now().Add(time.Duration(response.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	storage.LastRefresh = time.Now().UTC().Format(time.RFC3339)
	return nil
}

// HTTPStatusError retains the upstream status for auth refresh classification.
type HTTPStatusError struct {
	Code    int
	Message string
}

func (e *HTTPStatusError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}
func (e *HTTPStatusError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.Code
}

func closeKiroBody(body io.Closer, label string) {
	if body == nil {
		return
	}
	if err := body.Close(); err != nil {
		log.WithError(err).Debugf("close Kiro %s response", label)
	}
}

// Small wrappers keep the PKCE implementation private to this package while
// allowing LoginSession.CodeChallenge to remain a pure value operation.
func sha256Bytes(data []byte) []byte  { sum := sha256.Sum256(data); return sum[:] }
func base64RawURL(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }
