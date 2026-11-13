package executor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestClaudeNativeManagedBodyBetaParity(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "count"} {
		for _, beta := range []string{claudeStructuredOutputsBeta, claudeThinkingResumptionBeta, claudeInlineToolsBeta} {
			t.Run(mode+"/"+beta, func(t *testing.T) {
				incoming := nativeFidelityHeaders()
				incoming.Set("Anthropic-Beta", claudeCodeBeta)
				payload := []byte(fmt.Sprintf(`{"model":"claude-sonnet-5-5","max_tokens":16,"betas":[%q],"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"{\"device_id\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"account_uuid\":\"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb\",\"session_id\":\"11111111-2222-4333-8444-555555555555\"}"}}`, beta))
				_, headers := captureNativeFidelity(t, mode, &config.Config{}, payload, incoming)
				if !strings.Contains(headers.Get("Anthropic-Beta"), beta) {
					t.Errorf("native body beta %s was lost; outbound=%q", beta, headers.Get("Anthropic-Beta"))
				}
			})
		}
	}
}
