package cliproxy

import (
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestClaudeReferenceTokenUsesCurrentDirectOAuthCredential(t *testing.T) {
	now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	credential := func(id, token string) *coreauth.Auth {
		return &coreauth.Auth{
			ID: id, Provider: "claude", Status: coreauth.StatusActive,
			Metadata: map[string]any{"access_token": token, "expired": now.Add(time.Hour).Format(time.RFC3339)},
		}
	}
	old := credential("old", "sk-ant-oat-old")
	newer := credential("newer", "sk-ant-oat-new")
	newer.LastRefreshedAt = now.Add(time.Minute)
	foreign := credential("foreign", "sk-ant-oat-foreign")
	foreign.Attributes = map[string]string{"base_url": "https://gateway.example"}
	apiKeyOverride := credential("override", "sk-ant-oat-override")
	apiKeyOverride.Attributes = map[string]string{"api_key": "sk-ant-api03-other"}
	disabled := credential("disabled", "sk-ant-oat-disabled")
	disabled.Disabled = true
	expired := credential("expired", "sk-ant-oat-expired")
	expired.Metadata["expired"] = now.Add(-time.Minute).Format(time.RFC3339)
	unavailable := credential("unavailable", "sk-ant-oat-unavailable")
	unavailable.Unavailable = true
	unavailable.NextRetryAfter = now.Add(time.Hour)
	quotaBlocked := credential("quota", "sk-ant-oat-quota")
	quotaBlocked.Quota = coreauth.QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour)}

	if got := claudeReferenceToken([]*coreauth.Auth{old, foreign, apiKeyOverride, disabled, expired, unavailable, quotaBlocked, newer}, now); got != "sk-ant-oat-new" {
		t.Fatalf("selected token = %q, want newest eligible OAuth token", got)
	}
	if got := claudeReferenceToken([]*coreauth.Auth{foreign, apiKeyOverride, disabled, expired, unavailable, quotaBlocked}, now); got != "" {
		t.Fatalf("selected ineligible token %q", got)
	}
}

func TestDirectAnthropicBaseForReference(t *testing.T) {
	for _, raw := range []string{"", "https://api.anthropic.com", "https://api.anthropic.com:443/"} {
		if !isDirectAnthropicBase(raw) {
			t.Errorf("rejected direct base %q", raw)
		}
	}
	for _, raw := range []string{"http://api.anthropic.com", "https://api.anthropic.com.evil.test", "https://api.anthropic.com:8443", "https://api.anthropic.com/v1", "https://api.anthropic.com?x=1", "https://user@api.anthropic.com"} {
		if isDirectAnthropicBase(raw) {
			t.Errorf("accepted non-direct base %q", raw)
		}
	}
}
