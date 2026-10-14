package auth

import "strings"

// Quota recovery identities. A credential's quota recovery window is keyed by
// one of these identities rather than by its adapter: management's usage probes
// and greedy-soonest-recovery signal lookup both resolve the same value, so an
// OpenCode credential served through another adapter is still probed and read
// as OpenCode.
const (
	quotaRecoveryProviderZai        = "zai"
	quotaRecoveryProviderOpenCodeGo = "opencode-go"
	quotaRecoveryProviderCodex      = "codex"
	quotaRecoveryProviderClaude     = "claude"
	quotaRecoveryProviderKiro       = "kiro"
)

// QuotaRecoveryProviderForAuth resolves the provider identity that owns a
// credential's quota recovery window. It returns an empty string when the
// credential has no usable identity.
//
// A non-empty usage_probe attribute is authoritative and overrides the
// credential's adapter, so an OpenCode credential served through the Codex
// adapter still probes OpenCode. Only the supported probe values ("zai" and
// "opencode-go") are honored: any other explicit value resolves to the empty
// identity instead of falling back to the adapter, so an unrecognized probe can
// never send the credential to whichever upstream the adapter names.
//
// Without an override the credential's provider name is normalized, falling
// back to the provider_key and compat_name attributes set by the config
// synthesizer for OpenAI-compatible entries. Model names are never consulted.
func QuotaRecoveryProviderForAuth(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if probe := strings.ToLower(strings.TrimSpace(auth.Attributes["usage_probe"])); probe != "" {
		if probe == quotaRecoveryProviderZai || probe == quotaRecoveryProviderOpenCodeGo {
			return probe
		}
		return ""
	}
	for _, candidate := range []string{auth.Provider, auth.Attributes["provider_key"], auth.Attributes["compat_name"]} {
		if identity := quotaRecoveryIdentityForProviderName(candidate); identity != "" {
			return identity
		}
	}
	return ""
}

// quotaRecoveryIdentityForProviderName maps a provider or compatibility alias
// onto a quota recovery identity. Matching ignores case and separators so
// "OpenCode Go" and "openai-compatible-opencode-go" behave the same.
func quotaRecoveryIdentityForProviderName(name string) string {
	compact := quotaRecoveryCompactName(name)
	switch {
	case compact == "":
		return ""
	case compact == quotaRecoveryProviderCodex:
		return quotaRecoveryProviderCodex
	case compact == quotaRecoveryProviderClaude:
		return quotaRecoveryProviderClaude
	case compact == quotaRecoveryProviderKiro:
		return quotaRecoveryProviderKiro
	case strings.Contains(compact, "opencode") && strings.Contains(compact, "go"):
		return quotaRecoveryProviderOpenCodeGo
	case strings.Contains(compact, "zai"):
		return quotaRecoveryProviderZai
	}
	return ""
}

func quotaRecoveryCompactName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.NewReplacer(".", "", "-", "", "_", "", " ", "").Replace(name)
}
