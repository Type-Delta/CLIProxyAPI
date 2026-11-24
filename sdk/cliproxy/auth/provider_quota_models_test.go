package auth

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestProviderQuotaModelAliasIdentityAndKeys(t *testing.T) {
	t.Run("oauth prefix and suffix", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		m.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"claude": {{Name: "claude-fable-5-1", Alias: "family"}}})
		auth := &Auth{ID: "quota-oauth", Provider: "claude", Prefix: "team"}
		upstream, keys := m.ProviderQuotaModel(auth, "team/family(high)")
		if upstream != "claude-fable-5-1" || len(keys) == 0 || keys[0] != "claude-fable-5-1" {
			t.Fatalf("identity %q keys %v", upstream, keys)
		}
	})
	t.Run("api key", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		m.SetConfig(&internalconfig.Config{ClaudeKey: []internalconfig.ClaudeKey{{APIKey: "key", Models: []internalconfig.ClaudeModel{{Name: "claude-fable-5-1", Alias: "family"}}}}})
		auth := &Auth{ID: "quota-api", Provider: "claude", Attributes: map[string]string{"api_key": "key"}}
		upstream, keys := m.ProviderQuotaModel(auth, "family")
		if upstream != "claude-fable-5-1" || len(keys) == 0 || keys[0] != "family" {
			t.Fatalf("identity %q keys %v", upstream, keys)
		}
	})
}
