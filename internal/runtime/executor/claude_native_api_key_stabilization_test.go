package executor

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestClaudeNativeAPIKeyBillingSnapshot(t *testing.T) {
	for _, mode := range []string{"execute", "stream"} {
		for _, stable := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/stable=%v", mode, stable), func(t *testing.T) {
				resetClaudeDeviceProfileCache()
				t.Cleanup(resetClaudeDeviceProfileCache)
				cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &stable}}
				for _, version := range []string{"2.1.322", "2.1.318"} {
					incoming := nativeFidelityHeaders()
					incoming.Set("User-Agent", "claude-cli/"+version+" (external, sdk-ts)")
					incoming.Set("X-Stainless-Package-Version", "0.128.0")
					incoming.Set("X-Stainless-Runtime-Version", "v26.12.1")
					payload := []byte(fmt.Sprintf(`{"model":"claude-sonnet-5-5","max_tokens":16,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=%s.abc; cc_entrypoint=sdk-ts;"}],"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"{\"device_id\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"account_uuid\":\"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb\",\"session_id\":\"11111111-2222-4333-8444-555555555555\"}"}}`, version))
					var errSign error
					payload, errSign = finalizeAnthropicMessagesBodyCCH(payload, "")
					if errSign != nil {
						t.Fatal(errSign)
					}
					body, headers := captureNativeFidelity(t, mode, cfg, payload, incoming, map[string]string{"api_key": "sk-ant-api03-qa-synthetic"})
					billing := ojson.GetBytes(body, "system.0.text").String()
					signed, errResign := signAnthropicMessagesBody(body)
					if errResign != nil || !bytes.Equal(signed, body) {
						t.Errorf("native API-key signature became invalid: %v", errResign)
					}
					wantVersion := version
					if stable {
						wantVersion = "2.1.322"
					}
					if !strings.Contains(headers.Get("User-Agent"), wantVersion) || !strings.Contains(billing, "cc_version="+wantVersion+".") {
						t.Errorf("mixed stabilized profile after incoming%s: UA=%q billing=%q", version, headers.Get("User-Agent"), billing)
					}
				}
			})
		}
	}
}
