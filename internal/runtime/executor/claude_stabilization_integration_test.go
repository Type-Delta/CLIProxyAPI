package executor

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestClaudeStabilizationCoherentAcrossPaths(t *testing.T) {
	for _, stable := range []bool{false, true} {
		t.Run(fmt.Sprint(stable), func(t *testing.T) {
			resetClaudeDeviceProfileCache()
			t.Cleanup(resetClaudeDeviceProfileCache)
			cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &stable}}
			for i, step := range []struct{ mode, version, sdk, node, entry string }{
				{"execute", "2.1.318", "0.128.0", "v26.12.1", "sdk-ts"},
				{"stream", "2.1.322", "0.129.0", "v27.0.0", "sdk-ts"},
				{"count", "2.1.318", "0.128.0", "v26.12.1", "sdk-ts"},
				{"execute", "2.1.323", "0.130.0", "v28.0.0", "cli"},
			} {
				h := nativeFidelityHeaders()
				h.Del("X-Claude-Code-Agent-Id")
				ua := "claude-cli/" + step.version + " (external, " + step.entry + ", agent-sdk/0.3.316)"
				h.Set("User-Agent", ua)
				h.Set("X-Stainless-Package-Version", step.sdk)
				h.Set("X-Stainless-Runtime-Version", step.node)
				h.Set("X-Stainless-Os", "Linux")
				h.Set("X-Stainless-Arch", "x64")
				doc := ojson.NewDocString(`{"model":"claude-sonnet-5-5","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"{\"device_id\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"account_uuid\":\"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb\",\"session_id\":\"11111111-2222-4333-8444-555555555555\"}"}}`)
				_ = doc.Set("system", []map[string]string{{"type": "text", "text": "x-anthropic-billing-header: cc_version=" + step.version + ".abc; cc_entrypoint=" + step.entry + "; cch=00000;"}})
				body, out := captureNativeFidelity(t, step.mode, cfg, doc.Bytes(), h)
				wantVersion, wantSDK, wantNode, wantEntry, wantOS, wantArch := step.version, step.sdk, step.node, step.entry, "Linux", "x64"
				if stable {
					wantOS, wantArch = "MacOS", "arm64"
					if i >= 2 {
						wantVersion, wantSDK, wantNode, wantEntry = "2.1.322", "0.129.0", "v27.0.0", "sdk-ts"
					}
				}
				wantUA := "claude-cli/" + wantVersion + " (external, " + wantEntry + ", agent-sdk/0.3.316)"
				assertClaudeFingerprint(t, out, wantUA, wantSDK, wantNode, wantOS, wantArch)
				if step.mode == "count" {
					if ojson.GetBytes(body, "system").Exists() {
						t.Fatal("count_tokens gained billing")
					}
				} else {
					billing := ojson.GetBytes(body, "system.0.text").String()
					wantSuffix := "abc"
					if stable && i >= 2 {
						wantSuffix = computeFingerprint("hello", wantVersion)
					}
					if !strings.Contains(billing, "cc_version="+wantVersion+"."+wantSuffix+";") || !strings.Contains(billing, "cc_entrypoint="+wantEntry+";") {
						t.Fatalf("incoherent billing at step%d: %s", i, billing)
					}
					signed, err := signAnthropicMessagesBody(body)
					if err != nil || !bytes.Equal(signed, body) {
						t.Fatalf("final CCH is invalid at step%d: %v", i, err)
					}
				}
			}
		})
	}
}

func TestClaudeStabilizationRequestSnapshotSurvivesUpgrade(t *testing.T) {
	resetClaudeDeviceProfileCache()
	t.Cleanup(resetClaudeDeviceProfileCache)
	stable := true
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &stable}}
	credential := &auth.Auth{ID: "request-snapshot", Metadata: claudeOAuthTestMetadata()}
	first := http.Header{"User-Agent": {"claude-cli/2.1.318 (external, sdk-ts)"}, "X-Stainless-Package-Version": {"0.128.0"}, "X-Stainless-Runtime-Version": {"v26.12.1"}}
	ctx, err := helps.PrepareClaudeDeviceProfile(t.Context(), credential, "key", first, cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	newer := first.Clone()
	newer.Set("User-Agent", "claude-cli/2.1.322 (external, sdk-ts)")
	if _, err := helps.ResolveClaudeDeviceProfileRequired(t.Context(), credential, "key", newer, cfg); err != nil {
		t.Fatal(err)
	}
	profile, found := helps.ClaudeDeviceProfileFromContext(ctx)
	if !found {
		t.Fatal("missing request profile")
	}
	doc := ojson.NewDocString(`{"system":[{"text":"x-anthropic-billing-header: cc_version=2.1.318.abc; cc_entrypoint=sdk-ts;"}]}`)
	helps.ApplyClaudeDeviceProfileBillingDoc(doc, profile, "")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyClaudeHeaders(req, credential, "sk-ant-oat-snapshot", false, nil, doc.Bytes(), cfg, first, true); err != nil {
		t.Fatal(err)
	}
	assertClaudeFingerprint(t, req.Header, "claude-cli/2.1.318 (external, sdk-ts)", "0.128.0", "v26.12.1", "MacOS", "arm64")
	next, err := helps.ResolveClaudeDeviceProfileRequired(t.Context(), credential, "key", first, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if next.UserAgent != "claude-cli/2.1.322 (external, sdk-ts)" {
		t.Fatalf("stored profile downgraded: %+v", next)
	}
}

func TestClaudeStabilizationRecomputesVersionFingerprint(t *testing.T) {
	resetClaudeDeviceProfileCache()
	t.Cleanup(resetClaudeDeviceProfileCache)
	stable := true
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &stable}}
	for _, version := range []string{"2.1.322", "2.1.318"} {
		h := nativeFidelityHeaders()
		h.Set("User-Agent", "claude-cli/"+version+" (external, sdk-ts)")
		h.Set("X-Stainless-Package-Version", "0.128.0")
		h.Set("X-Stainless-Runtime-Version", "v26.12.1")
		payload := []byte(fmt.Sprintf(`{"model":"claude-sonnet-5-5","max_tokens":16,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=sdk-ts; cch=00000;"}],"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"{\"device_id\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"account_uuid\":\"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb\",\"session_id\":\"11111111-2222-4333-8444-555555555555\"}"}}`, version, computeFingerprint("hello", version)))
		body, _ := captureNativeFidelity(t, "execute", cfg, payload, h)
		if got := ojson.GetBytes(body, "system.0.text").String(); !strings.Contains(got, "cc_version=2.1.322."+computeFingerprint("hello", "2.1.322")+";") {
			t.Errorf("version fingerprint is inconsistent after %s: %s", version, got)
		}
	}
	body, outgoing := captureNativeFidelity(t, "execute", cfg, []byte(`{"model":"claude-sonnet-5-5","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`), http.Header{"User-Agent": {"audit-client/1.0"}})
	billing := ojson.GetBytes(body, "system.0.text").String()
	if !strings.Contains(billing, "cc_version=2.1.322."+computeFingerprint("hello", "2.1.322")+";") || !strings.Contains(outgoing.Get("User-Agent"), "2.1.322") {
		t.Fatalf("reconstructed request did not use coherent learned version/fingerprint: %s", billing)
	}

}

func TestClaudeSignatureDistinctCredentialsPreserveIdentity(t *testing.T) {
	resetClaudeDeviceProfileCache()
	t.Cleanup(resetClaudeDeviceProfileCache)
	stable := true
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &stable}}
	h := nativeFidelityHeaders()
	h.Del("X-Claude-Code-Agent-Id")
	h.Set("X-Stainless-Package-Version", "0.128.0")
	h.Set("X-Stainless-Runtime-Version", "v26.12.1")
	payload := []byte(`{"model":"claude-sonnet-5-5","max_tokens":16,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.322.abc; cc_entrypoint=sdk-ts; cch=00000;"}],"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"{\"device_id\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"account_uuid\":\"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb\",\"session_id\":\"11111111-2222-4333-8444-555555555555\"}"}}`)
	// The capture helper accepts the same metadata as a deliberate duplicated
	// credential input. Distinct auth scopes must still have distinct devices.
	devices := map[string]string{}
	for _, scope := range []string{"credential-one", "credential-two", "credential-one"} {
		account := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		if scope == "credential-two" {
			account = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		}
		body, headers := captureNativeFidelity(t, "execute", cfg, payload, h, map[string]string{"audit_auth_id": scope, "audit_account_uuid": account})
		device := ojson.Get(ojson.GetBytes(body, "metadata.user_id").String(), "device_id").String()
		if previous, seen := devices[scope]; seen && previous != device {
			t.Fatal("credential device changed between requests")
		}
		devices[scope] = device
		assertClaudeCredentialIdentity(t, body, headers, []string{device}, account)
	}
	if devices["credential-one"] == devices["credential-two"] {
		t.Fatal("two credential signatures share a device")
	}
	cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"metadata"}}}
	body, _ := captureNativeFidelity(t, "count", cfg, payload, h)
	if ojson.GetBytes(body, "metadata").Exists() {
		t.Fatal("signature normalization restored filtered count-token metadata")
	}
}

func TestClaudeSignatureCustomFallbackDispatches(t *testing.T) {
	resetClaudeDeviceProfileCache()
	t.Cleanup(resetClaudeDeviceProfileCache)
	enabled := true
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &enabled, UserAgent: "my-gateway/1.0", PackageVersion: "custom-pkg", RuntimeVersion: "custom-runtime"}}
	body, h := captureNativeFidelity(t, "execute", cfg, []byte(`{"model":"claude-sonnet-5-5","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`), http.Header{"User-Agent": {"fixture/1.0"}})
	if h.Get("User-Agent") != "my-gateway/1.0" || len(body) == 0 {
		t.Fatal("custom fallback dispatch changed")
	}
}

func TestClaudeSignatureAliasesAndTogglesPreserveStoredDevice(t *testing.T) {
	resetClaudeDeviceProfileCache()
	t.Cleanup(resetClaudeDeviceProfileCache)
	headers := nativeFidelityHeaders()
	headers.Set("X-Stainless-Package-Version", "0.128.0")
	headers.Set("X-Stainless-Runtime-Version", "v26.12.1")
	doc := ojson.NewDocString(`{"model":"claude-sonnet-5-5","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
	_ = doc.Set("metadata.user_id", `{"device_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","account_uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","session_id":"11111111-2222-4333-8444-555555555555"}`)
	payload := doc.Bytes()
	for _, test := range []struct {
		id      string
		enabled bool
	}{{"a.json", false}, {"a.json", true}, {"renamed-a.json", true}, {"copy-a.json", true}, {"renamed-a.json", false}} {
		cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &test.enabled}}
		body, _ := captureNativeFidelity(t, "execute", cfg, payload, headers, map[string]string{"audit_auth_id": test.id})
		device := ojson.Get(ojson.GetBytes(body, "metadata.user_id").String(), "device_id").String()
		if device != strings.Repeat("b", 64) {
			t.Fatalf("file%s stable%v changed stored device: %s", test.id, test.enabled, device)
		}
	}
}
