package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	tr "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func captureNativeFidelity(t *testing.T, mode string, cfg *config.Config, payload []byte, headers http.Header, attributes ...map[string]string) ([]byte, http.Header) {
	t.Helper()
	var body []byte
	var outgoing http.Header
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		outgoing = make(http.Header)
		for key, values := range r.Header {
			for _, value := range values {
				outgoing.Add(key, value)
			}
		}
		response := `{"id":"msg_new","type":"message","model":"claude-sonnet-5-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`
		if mode == "count" {
			response = `{"input_tokens":7}`
		} else if mode == "stream" {
			response = "data: {\"type\":\"message_stop\"}\n\n"
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	a := &auth.Auth{ID: "native-fidelity-B", Attributes: map[string]string{"api_key": "sk-ant-oat-native-fidelity-B"}, Metadata: map[string]any{"account_uuid": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "claude_device_ids": []string{strings.Repeat("b", 64)}}}
	for _, attrs := range attributes {
		for key, value := range attrs {
			if key == "audit_auth_id" {
				a.ID = value
				continue
			}
			if key == "audit_account_uuid" {
				a.Metadata["account_uuid"] = value
				continue
			}
			a.Attributes[key] = value
		}
	}
	engine := NewClaudeExecutor(cfg)
	req := ex.Request{Model: "claude-sonnet-5-5", Payload: payload}
	opts := ex.Options{SourceFormat: tr.FormatClaude, OriginalRequest: payload, Headers: headers}
	var err error
	switch mode {
	case "count":
		_, err = engine.CountTokens(ctx, a, req, opts)
	case "stream":
		var result *ex.StreamResult
		result, err = engine.ExecuteStream(ctx, a, req, opts)
		if err == nil {
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
			}
		}
	default:
		_, err = engine.Execute(ctx, a, req, opts)
	}
	if err != nil {
		t.Fatal(err)
	}
	if body == nil {
		t.Fatal("no outbound capture")
	}
	return body, outgoing
}

func nativeFidelityHeaders() http.Header {
	return http.Header{"User-Agent": {"claude-cli/2.1.322 (external, sdk-ts, agent-sdk/0.3.316)"}, "X-App": {"cli"}, "Anthropic-Beta": {"claude-code-20250219,interleaved-thinking-2025-05-14,context-management-2025-06-27,effort-2025-11-24"}, "X-Claude-Code-Session-Id": {"11111111-2222-4333-8444-555555555555"}, "X-Claude-Code-Agent-Id": {"worker-123"}}
}

func TestClaudeNativeFidelitySessionAndFeatures(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "count"} {
		for _, affinity := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/affinity=%v", mode, affinity), func(t *testing.T) {
				off := false
				cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &off}}
				cfg.Routing.SessionAffinitySubagents = &affinity
				body, headers := captureNativeFidelity(t, mode, cfg, []byte(`{"model":"claude-sonnet-5-5","max_tokens":64,"output_config":{"effort":"low"},"thinking":{"type":"adaptive"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"prior","signature":"`+preservedThinkingTestSignature+`"}]},{"role":"user","content":"continue"}],"metadata":{"user_id":"{\"device_id\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"account_uuid\":\"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb\",\"session_id\":\"11111111-2222-4333-8444-555555555555\"}"}}`), nativeFidelityHeaders())
				if got := headers.Get("X-Claude-Code-Session-Id"); got != "11111111-2222-4333-8444-555555555555" {
					t.Errorf("wire root session = %q", got)
				}
				if mode != "count" {
					if got := ojson.Get(ojson.GetBytes(body, "metadata.user_id").String(), "session_id").String(); got != "11111111-2222-4333-8444-555555555555" {
						t.Errorf("metadata root session = %q", got)
					}
				}
				if ojson.GetBytes(body, "thinking.block_binding").Exists() {
					t.Errorf("native history received drop_block")
				}
				if !strings.Contains(headers.Get("Anthropic-Beta"), claudeEffortBeta) {
					t.Error("native effort beta dropped")
				}
			})
		}
	}
}

func TestClaudeNativeFidelityBodyBetasFollowFinalRules(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "count"} {
		for _, filter := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/filter=%v", mode, filter), func(t *testing.T) {
				cfg := &config.Config{}
				if filter {
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"betas"}}}
				} else {
					cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"betas": []string{"message-threads-2026-08-12"}}}}
				}
				body, h := captureNativeFidelity(t, mode, cfg, []byte(`{"model":"claude-sonnet-5-5","max_tokens":16,"betas":["message-threads-2026-08-12"],"messages":[{"role":"user","content":"hello"}]}`), nativeFidelityHeaders())
				if ojson.GetBytes(body, "betas").Exists() {
					t.Error("body betas were not lifted")
				}
				if got := strings.Contains(h.Get("Anthropic-Beta"), "message-threads-2026-08-12"); got == filter {
					t.Errorf("body beta header present=%v, filter=%v", got, filter)
				}
			})
		}
	}
}

func TestClaudeNativeFidelityCredentialContinuity(t *testing.T) {
	for _, mode := range []string{"execute", "stream"} {
		for _, account := range []string{"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ""} {
			t.Run(mode+"/"+account, func(t *testing.T) {
				payload := []byte(fmt.Sprintf(`{"model":"claude-sonnet-5-5","max_tokens":16,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.322.abc; cc_entrypoint=sdk-ts; cch=00000; cc_prev_req=req_previous; cc_prompt_id=11111111-2222-4333-8444-555555555555;"}],"messages":[{"role":"user","content":"hello"}],"diagnostics":{"previous_message_id":"msg_previous"},"thread":{"previous_message_id":"msg_previous"},"metadata":{"user_id":"{\"device_id\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"account_uuid\":\"%s\",\"session_id\":\"11111111-2222-4333-8444-555555555555\"}"}}`, account))
				body, _ := captureNativeFidelity(t, mode, &config.Config{}, payload, nativeFidelityHeaders())
				switched := strings.HasPrefix(account, "aaaaaaaa")
				for _, path := range []string{"diagnostics.previous_message_id", "thread.previous_message_id"} {
					if got := ojson.GetBytes(body, path).Exists(); got == switched {
						t.Errorf("%s present=%v switched=%v", path, got, switched)
					}
				}
				if got := strings.Contains(ojson.GetBytes(body, "system.0.text").String(), "cc_prev_req="); got == switched {
					t.Errorf("cc_prev_req present=%v switched=%v", got, switched)
				}
				if !strings.Contains(ojson.GetBytes(body, "system.0.text").String(), "cc_prompt_id=11111111-2222-4333-8444-555555555555;") {
					t.Error("credential continuity reconciliation removed the prompt identity")
				}
			})
		}
	}
}

func TestClaudeNativeFidelityExplicitNeverAndReentry(t *testing.T) {
	payload := []byte(`{"model":"claude-sonnet-5-5","max_tokens":16,"output_config":{"effort":"low"},"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"{\"device_id\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"account_uuid\":\"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb\",\"session_id\":\"11111111-2222-4333-8444-555555555555\"}"}}`)
	for _, mode := range []string{"execute", "stream", "count"} {
		t.Run(mode, func(t *testing.T) {
			off := false
			cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &off}}
			for attempt := 0; attempt < 2; attempt++ {
				body, h := captureNativeFidelity(t, mode, cfg, payload, nativeFidelityHeaders(), map[string]string{"cloak_mode": "never"})
				if h.Get("X-Claude-Code-Session-Id") != "11111111-2222-4333-8444-555555555555" {
					t.Error("root session changed on reentry")
				}
				if !strings.Contains(h.Get("Anthropic-Beta"), claudeEffortBeta) {
					t.Error("native effort beta dropped")
				}
				if ojson.GetBytes(body, "thinking.block_binding").Exists() {
					t.Error("native thinking binding injected")
				}
			}
		})
	}
}

func TestClaudeNativeFidelityContinuityPayloadOverridesWin(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"diagnostics.previous_message_id": "msg_override", "thread.previous_message_id": "msg_override", "system.0.text": "x-anthropic-billing-header: cc_prev_req=req_override;"}}}}}
	payload := []byte(`{"model":"claude-sonnet-5-5","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"{\"device_id\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"account_uuid\":\"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa\",\"session_id\":\"11111111-2222-4333-8444-555555555555\"}"}}`)
	for _, mode := range []string{"execute", "stream"} {
		t.Run(mode, func(t *testing.T) {
			body, _ := captureNativeFidelity(t, mode, cfg, payload, nativeFidelityHeaders())
			for _, path := range []string{"diagnostics.previous_message_id", "thread.previous_message_id"} {
				if ojson.GetBytes(body, path).String() != "msg_override" {
					t.Errorf("final %s overwritten", path)
				}
			}
			if !strings.Contains(ojson.GetBytes(body, "system.0.text").String(), "cc_prev_req=req_override;") {
				t.Error("final billing previous request overwritten")
			}
		})
	}
}

func TestClaudeNativeFidelityUnmanagedBetaParity(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "count"} {
		for _, bodyBeta := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/body=%v", mode, bodyBeta), func(t *testing.T) {
				headers := http.Header{"User-Agent": {"AuditClient/1.0"}}
				payload := `{"model":"claude-sonnet-5-5","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`
				if bodyBeta {
					payload = `{"model":"claude-sonnet-5-5","max_tokens":16,"betas":["message-threads-2026-08-12","token-counting-2024-11-01"],"messages":[{"role":"user","content":"hello"}]}`
				} else {
					headers.Set("Anthropic-Beta", "message-threads-2026-08-12,token-counting-2024-11-01")
				}
				_, h := captureNativeFidelity(t, mode, &config.Config{}, []byte(payload), headers)
				if !strings.Contains(h.Get("Anthropic-Beta"), "message-threads-2026-08-12") {
					t.Error("unmanaged requested beta lost")
				}
				if mode != "count" && strings.Contains(h.Get("Anthropic-Beta"), claudeTokenCountingBeta) {
					t.Error("managed incompatible token-counting beta copied")
				}
			})
		}
	}
}

func TestClaudeNativeFidelityKnownCredentialSwitchAdaptsSignedHistory(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "count"} {
		t.Run(mode, func(t *testing.T) {
			doc := ojson.NewDoc(preservedThinkingReplayPayload("claude-sonnet-5-5"))
			_ = doc.Set("metadata.user_id", `{"device_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","account_uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","session_id":"11111111-2222-4333-8444-555555555555"}`)
			body, _ := captureNativeFidelity(t, mode, &config.Config{}, doc.Bytes(), nativeFidelityHeaders())
			if ojson.GetBytes(body, "thinking.block_binding.prefix_mismatch_behavior").String() != "drop_block" {
				t.Error("known credential switch did not adapt signed thinking history")
			}
		})
	}
}

func TestClaudeNativeFidelityBodyEffortBeta(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "count"} {
		t.Run(mode, func(t *testing.T) {
			doc := ojson.NewDoc([]byte(`{"model":"claude-sonnet-5-5","max_tokens":16,"output_config":{"effort":"low"},"betas":["effort-2025-11-24"],"messages":[{"role":"user","content":"hello"}]} `))
			_ = doc.Set("metadata.user_id", `{"device_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","account_uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","session_id":"11111111-2222-4333-8444-555555555555"}`)
			headers := nativeFidelityHeaders()
			headers.Set("Anthropic-Beta", claudeCodeBeta)
			_, h := captureNativeFidelity(t, mode, &config.Config{}, doc.Bytes(), headers)
			if !strings.Contains(h.Get("Anthropic-Beta"), claudeEffortBeta) {
				t.Error("native requested body effort beta dropped")
			}
		})
	}
}
