package claudecapture

import (
	"net/http"
	"testing"
)

func TestProfileHeadersNativeUserAgentExtensions(t *testing.T) {
	p := referenceFixture()
	h := http.Header{}
	h.Set("Anthropic-Version", "2023-06-01")
	h.Set("Anthropic-Beta", "oauth-2025-04-20")
	for _, ua := range []string{"claude-cli/2.1.322 (external, cli, workload/cron)", "claude-cli/2.1.322 (external, sdk-ts, workload/cron, agent-sdk/0.3.220)"} {
		h.Set("User-Agent", ua)
		if err := p.CheckHeaders(h); err != nil {
			t.Fatal(err)
		}
	}
	h.Set("User-Agent", "claude-cli/2.1.322 (external, cli, workload/)")
	if err := p.CheckHeaders(h); err == nil {
		t.Fatal("invalid UA extension accepted")
	}
}
