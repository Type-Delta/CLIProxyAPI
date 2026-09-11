package management

import (
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func credentialDisplayName(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if label := strings.TrimSpace(authAttribute(auth, "label")); label != "" {
		return label
	}
	if name := filepath.Base(strings.TrimSpace(strings.ReplaceAll(auth.FileName, "\\", "/"))); name != "" && name != "." && name != string(filepath.Separator) {
		return name
	}

	provider := strings.TrimSpace(auth.Provider)
	label := strings.TrimSpace(auth.Label)
	kind := strings.ToLower(strings.TrimSpace(auth.AuthKind()))
	if label != "" && (kind != coreauth.AuthKindAPIKey && kind != "api_key" || !isGenericCredentialLabel(label, provider)) {
		return label
	}

	apiKey := strings.TrimSpace(authAttribute(auth, coreauth.AttributeAPIKey))
	if apiKey != "" {
		return strings.TrimSpace(provider + " " + util.HideAPIKey(apiKey))
	}
	return strings.TrimSpace(auth.ID)
}

func isGenericCredentialLabel(label, provider string) bool {
	label = strings.ToLower(strings.TrimSpace(label))
	provider = strings.ToLower(strings.TrimSpace(provider))
	return label == provider+"-apikey" || label == provider+"-api-key"
}
