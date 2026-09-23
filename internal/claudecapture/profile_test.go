package claudecapture

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func referenceFixture() *Profile {
	return &Profile{
		SchemaVersion: schemaVersion,
		ClaudeVersion: "2.1.269",
		CapturedAt:    time.Now().UTC(),
		Headers: map[string]string{
			"User-Agent":        "claude-cli/2.1.269 (external, sdk-cli)",
			"Anthropic-Version": "2023-06-01",
		},
		Variants: []Variant{{Endpoint: "messages", Stream: true, Beta: "claude-code-20250219,oauth-2025-04-20", BodyKeys: []string{"messages", "model", "stream"}}},
	}
}

func TestProfileCheckRequestAndVersion(t *testing.T) {
	profile := referenceFixture()
	path := filepath.Join(t.TempDir(), "reference.json")
	if err := writeProfile(path, profile); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, "2.1.269")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, "2.1.270"); err == nil {
		t.Fatal("stale CLI version was accepted")
	}
	values := http.Header{}
	values.Set("User-Agent", profile.Headers["User-Agent"])
	values.Set("Anthropic-Version", "2023-06-01")
	values.Set("Anthropic-Beta", profile.Variants[0].Beta)
	body := []byte(`{"model":"claude-haiku","messages":[],"stream":true}`)
	if err := loaded.CheckRequest(values, body, "messages"); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(){
		"header": func() { values.Set("User-Agent", "other-client") },
		"beta":   func() { values.Set("Anthropic-Beta", "claude-code-20250219") },
	} {
		clone := values.Clone()
		change()
		if err := loaded.CheckRequest(values, body, "messages"); err == nil {
			t.Errorf("%s mismatch was accepted", name)
		}
		values = clone
	}
	if err := loaded.CheckRequest(values, body, "count_tokens"); err != nil {
		t.Fatalf("valid count_tokens shape was rejected: %v", err)
	}
	if err := loaded.CheckRequest(values, []byte(`{"model":"claude-haiku","messages":[],"stream":false}`), "messages"); err != nil {
		t.Fatalf("valid nonstream request was rejected: %v", err)
	}
	if err := loaded.CheckRequest(values, []byte(`{"model":"claude-haiku","messages":[],"stream":true,"metadata":{}}`), "messages"); err != nil {
		t.Fatalf("optional metadata was rejected: %v", err)
	}
	for _, invalid := range []string{`{"model":42,"messages":[]}`, `{"model":"claude-haiku","messages":{}}`, `{"model":"claude-haiku","messages":[],"stream":"yes"}`} {
		if err := loaded.CheckRequest(values, []byte(invalid), "messages"); err == nil {
			t.Errorf("invalid body was accepted: %s", invalid)
		}
	}
	if err := loaded.CheckRequest(values, body, "unknown"); err == nil {
		t.Fatal("unsupported endpoint was accepted")
	}
}

func TestProfileFromRequestDoesNotPersistRequestValues(t *testing.T) {
	secret := "secret-access-token-do-not-save"
	prompt := "private-prompt-do-not-save"
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("User-Agent", "claude-cli/2.1.269 (external, sdk-cli)")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20")
	body, err := json.Marshal(map[string]any{"model": "claude-haiku", "messages": []any{map[string]any{"role": "user", "content": prompt}}, "stream": true, "metadata": map[string]string{"user_id": "private-user"}})
	if err != nil {
		t.Fatal(err)
	}
	profile := profileFromRequest(req, body, "2.1.269")
	if profile == nil {
		t.Fatal("valid OAuth request was not captured")
	}
	path := filepath.Join(t.TempDir(), "reference.json")
	if err := writeProfile(path, profile); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secret, prompt, "private-user", "Authorization", "user_id"} {
		if strings.Contains(string(data), forbidden) {
			t.Errorf("profile persisted forbidden request value %q", forbidden)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("profile mode = %o, want 600", info.Mode().Perm())
	}
}

func TestCaptureEnvironmentForcesPrivateHomeAndProxy(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "old-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "old-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://example.com")
	env := privateCommandEnvironment("/cpa/.capture-test", "127.0.0.1:1234", "/cpa/.capture-test/ca.pem")
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{"old-key", "old-token", "https://example.com"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("capture inherited an API override: %s", forbidden)
		}
	}
	for _, required := range []string{"HOME=/cpa/.capture-test", "CLAUDE_CONFIG_DIR=/cpa/.capture-test/claude", "HTTPS_PROXY=http://127.0.0.1:1234", "NODE_EXTRA_CA_CERTS=/cpa/.capture-test/ca.pem"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("capture environment lacks %q", required)
		}
	}
}
