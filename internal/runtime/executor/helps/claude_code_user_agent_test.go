package helps

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClaudeCodeUserAgentExtensionGrammar(t *testing.T) {
	for _, ua := range []string{
		"claude-cli/2.1.322 (external, cli)",
		"claude-cli/2.1.322 (external, sdk-ts, workload/cron, agent-sdk/0.3.220)",
		"claude-cli/2.1.322 (external, cli, future-feature/value, workload/cron)",
	} {
		parsed, ok := parseClaudeCodeUserAgent(ua)
		if !ok {
			t.Fatalf("valid extension rejected: %q", ua)
		}
		stable, ok := parseClaudeCodeUserAgent(parsed.profileUserAgent)
		if !ok || len(stable.requestFields) != 0 {
			t.Fatalf("extension stored in identity: %+v", stable)
		}
	}
	for _, ua := range []string{
		"claude-cli/2.1.322 (external, cli, malformed)",
		"claude-cli/2.1.322 (external, cli, workload/)",
		"claude-cli/2.1.322 (external, cli, workload/cron, workload/again)",
		"claude-cli/2.1.322 (external, cli, Agent-SDK/0.3.220, agent-sdk/0.3.221)",
		"claude-cli/2.1.322 (external, cli, agent-sdk/dev)",
		"claude-cli/2.1.322 (external, cli, workload/(cron))",
		"claude-cli/2.1.322 (external, cli, workload/cron)\r\nInjected: value",
		"claude-cli/2.1.322(external, cli)",
	} {
		if ValidClaudeCodeUserAgent(ua) {
			t.Fatalf("malformed extension accepted: %q", ua)
		}
	}
}

func TestClaudeDeviceProfileRequestExtensionsNeverBecomeIdentity(t *testing.T) {
	profileStore(t, true)
	h := claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts, workload/cron, agent-sdk/0.3.220)")
	profile := resolveProfile(t, h, nil)
	if strings.Contains(profile.UserAgent, "workload/") {
		t.Fatal("workload stored")
	}
	for _, item := range []struct{ caller, want string }{
		{"claude-cli/2.1.318 (external, cli, agent-sdk/0.3.100, workload/cron)", "claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220, workload/cron)"},
		{"claude-cli/2.1.318 (external, cli, future-feature/next)", "claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220, future-feature/next)"},
		{"", profile.UserAgent},
	} {
		r := httptest.NewRequest("POST", "/messages", nil)
		ApplyClaudeDeviceProfileHeaders(r, profile, nil, item.caller)
		if got := r.Header.Get("User-Agent"); got != item.want {
			t.Fatalf("UA = %q, want %q", got, item.want)
		}
	}
	if got := resolveProfile(t, nil, nil); got != profile {
		t.Fatal("request extensions changed persistent identity")
	}
}

func TestDetectClaudeCodeRequestAcceptsCLIBackgroundExtensions(t *testing.T) {
	h := confirmedClaudeCodeHeaders()
	h.Set("X-App", "cli-bg")
	h.Set("User-Agent", "claude-cli/3.0.0 (external, cli, workload/cron)")
	if got := DetectClaudeCodeRequest(h, claudeCodeDetectionPayload(validClaudeCodeMetadataUserID), false); !got.Confirmed {
		t.Fatalf("cli-bg native request rejected: %+v", got)
	}
	h.Del("Anthropic-Beta")
	if got := DetectClaudeCodeRequest(h, claudeCodeDetectionPayload(validClaudeCodeMetadataUserID), false); got.Confirmed {
		t.Fatal("extension bypassed strong signals")
	}
}
