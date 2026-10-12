package kiro

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	log "github.com/sirupsen/logrus"
)

// TokenStorage is the normal file-backed OAuth representation used by Kiro.
// Kiro's native clients use camelCase fields; snake_case aliases are retained
// so imported files remain compatible with the shared auth synthesizer.
type TokenStorage struct {
	Type         string         `json:"type"`
	AccessToken  string         `json:"access_token,omitempty"`
	RefreshToken string         `json:"refresh_token,omitempty"`
	TokenType    string         `json:"token_type,omitempty"`
	ClientID     string         `json:"client_id,omitempty"`
	ClientSecret string         `json:"client_secret,omitempty"`
	ProfileARN   string         `json:"profile_arn,omitempty"`
	Region       string         `json:"region,omitempty"`
	AuthMethod   string         `json:"auth_method,omitempty"`
	ExpiresAt    string         `json:"expired,omitempty"`
	LastRefresh  string         `json:"last_refresh,omitempty"`
	Metadata     map[string]any `json:"-"`
}

// KiroTokenStorage is kept as a descriptive alias for callers that mirror
// other provider packages' storage type names.
type KiroTokenStorage = TokenStorage

// UnmarshalJSON accepts both Kiro's native camelCase file shape and the
// shared CPA snake_case credential shape.
func (s *TokenStorage) UnmarshalJSON(data []byte) error {
	type tokenAlias TokenStorage
	var raw struct {
		*tokenAlias
		AccessTokenCamel  string `json:"accessToken"`
		RefreshTokenCamel string `json:"refreshToken"`
		TokenTypeCamel    string `json:"tokenType"`
		ClientIDCamel     string `json:"clientId"`
		ClientSecretCamel string `json:"clientSecret"`
		ProfileARNCamel   string `json:"profileArn"`
		ExpiresAtCamel    string `json:"expiresAt"`
		AuthMethodCamel   string `json:"authMethod"`
	}
	raw.tokenAlias = (*tokenAlias)(s)
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if s.AccessToken == "" {
		s.AccessToken = raw.AccessTokenCamel
	}
	if s.RefreshToken == "" {
		s.RefreshToken = raw.RefreshTokenCamel
	}
	if s.TokenType == "" {
		s.TokenType = raw.TokenTypeCamel
	}
	if s.ClientID == "" {
		s.ClientID = raw.ClientIDCamel
	}
	if s.ClientSecret == "" {
		s.ClientSecret = raw.ClientSecretCamel
	}
	if s.ProfileARN == "" {
		s.ProfileARN = raw.ProfileARNCamel
	}
	if s.ExpiresAt == "" {
		s.ExpiresAt = raw.ExpiresAtCamel
	}
	if s.AuthMethod == "" {
		s.AuthMethod = raw.AuthMethodCamel
	}
	return nil
}

// SetMetadata injects auth-file metadata before persistence.
func (s *TokenStorage) SetMetadata(metadata map[string]any) { s.Metadata = metadata }

// SaveTokenToFile persists Kiro credentials with restrictive permissions.
func (s *TokenStorage) SaveTokenToFile(path string) error {
	if s == nil {
		return fmt.Errorf("Kiro token storage is nil")
	}
	misc.LogSavingCredentials(path)
	s.Type = Provider
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create Kiro auth directory: %w", err)
	}
	data := map[string]any{
		"type":          s.Type,
		"access_token":  s.AccessToken,
		"refresh_token": s.RefreshToken,
		"token_type":    s.TokenType,
		"client_id":     s.ClientID,
		"client_secret": s.ClientSecret,
		"profile_arn":   s.ProfileARN,
		"region":        s.Region,
		"auth_method":   s.AuthMethod,
		"expired":       s.ExpiresAt,
		"last_refresh":  s.LastRefresh,
	}
	for key, value := range s.Metadata {
		data[key] = value
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create Kiro token file: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			log.WithError(closeErr).Warn("close Kiro token file")
		}
	}()
	if err := json.NewEncoder(file).Encode(data); err != nil {
		return fmt.Errorf("write Kiro token file: %w", err)
	}
	return nil
}

// Expiry returns the parsed token expiry, if one is available.
func (s *TokenStorage) Expiry() time.Time {
	if s == nil {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, time.RFC3339Nano} {
		if parsed, err := time.Parse(layout, s.ExpiresAt); err == nil {
			return parsed
		}
	}
	return time.Time{}
}
