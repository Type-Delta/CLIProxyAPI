package helps

import (
	"net/http/httptest"
	"testing"
)

func TestClaudeProfileAlternatingBillingCannotDowngrade(t *testing.T) {
	profile := defaultClaudeDeviceProfile(nil)
	profile.UserAgent = "claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)"
	profile.PackageVersion = "0.128.0"
	profile.RuntimeVersion = "v26.12.1"
	for _, version := range []string{"2.1.322", "2.1.318"} {
		r := httptest.NewRequest("POST", "/messages", nil)
		ApplyClaudeDeviceProfileHeaders(r, profile, []byte(`{"system":[{"text":"x-anthropic-billing-header: cc_version=`+version+`.abc; cc_entrypoint=cli;"}]}`), "claude-cli/"+version+" (external, cli)")
		if got := r.Header.Get("User-Agent"); got != profile.UserAgent {
			t.Errorf("coherent stored tuple changed: got %q, want %q", got, profile.UserAgent)
		}
	}
}

func TestClaudeProfileNewerTupleIsAdopted(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)
	h := claudeDeviceHeaders("claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.220)")
	h.Set("X-Stainless-Package-Version", "0.128.0")
	h.Set("X-Stainless-Runtime-Version", "v26.12.1")
	p, err := resolveClaudeDeviceProfileLocal(nil, "regression", h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.UserAgent != h.Get("User-Agent") || p.PackageVersion != "0.128.0" || p.RuntimeVersion != "v26.12.1" {
		t.Fatalf("newer native tuple rejected: %+v", p)
	}
}
