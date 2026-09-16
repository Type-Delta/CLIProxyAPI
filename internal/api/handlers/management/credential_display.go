package management

import (
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type analyticsCredentialDisplay struct {
	label    *string
	filename *string
}

// analyticsCredentialDisplays resolves currently loaded credentials to admin-only display names.
// A non-empty provider scopes the lookup for event details; dimensions carry hashed IDs only.
func (h *Handler) analyticsCredentialDisplays(service cpauk.Service, provider string, ids []string) map[string]analyticsCredentialDisplay {
	if h == nil || service == nil || len(ids) == 0 {
		return nil
	}
	identityProvider, ok := service.(interface {
		CredentialID(provider, authIndex, authID string) (*string, error)
	})
	if !ok {
		return nil
	}
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			wanted[id] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return nil
	}
	result := make(map[string]analyticsCredentialDisplay, len(wanted))
	for _, credential := range manager.List() {
		if credential == nil || provider != "" && !strings.EqualFold(strings.TrimSpace(credential.Provider), provider) {
			continue
		}
		id, err := identityProvider.CredentialID(credential.Provider, credential.Index, credential.ID)
		if err != nil || id == nil {
			continue
		}
		if _, ok := wanted[*id]; !ok {
			continue
		}
		display := analyticsCredentialDisplay{}
		name := strings.TrimSpace(credential.FileName)
		if index := strings.LastIndexAny(name, `/\`); index >= 0 {
			name = name[index+1:]
		}
		if name != "" && name != "." && name != ".." {
			display.filename = &name
		}
		if label := credentialDisplayName(credential); label != "" {
			display.label = &label
		}
		result[*id] = display
	}
	return result
}

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
