package cliproxy

import (
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAntigravityReferenceCredentialsUseCurrentDirectOAuth(t *testing.T) {
	now := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	valid := &coreauth.Auth{
		Provider: "antigravity",
		Metadata: map[string]any{
			"access_token": "test-access", "project_id": "test-project", "expired": now.Add(time.Hour).Format(time.RFC3339),
		},
	}
	expired := valid.Clone()
	expired.Metadata["expired"] = now.Add(-time.Hour).Format(time.RFC3339)
	custom := valid.Clone()
	custom.Attributes = map[string]string{"base_url": "https://example.invalid"}
	disabled := valid.Clone()
	disabled.Disabled = true
	noProject := valid.Clone()
	delete(noProject.Metadata, "project_id")

	candidates := antigravityReferenceCredentials([]*coreauth.Auth{expired, custom, disabled, noProject, valid, valid}, "http://localhost:4000", now)
	if len(candidates) != 1 || candidates[0].AccessToken != "test-access" || candidates[0].Project != "test-project" ||
		candidates[0].ProxyURL != "http://localhost:4000" {
		t.Fatalf("unexpected reference candidates: %+v", candidates)
	}
}
