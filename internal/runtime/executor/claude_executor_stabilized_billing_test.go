package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestApplyClaudeHeaders_StabilizedHeadersUseObservedTuple(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
		StabilizeDeviceProfile: &stabilize,
		NativeEntrypoints:      []string{"t3-code"},
	}}
	auth := &cliproxyauth.Auth{ID: "stabilized-billing", Metadata: claudeOAuthTestMetadata()}
	const incomingUA = "claude-cli/2.1.319 (external, t3-code, agent-sdk/0.3.316)"

	tests := []struct {
		name      string
		billing   string
		confirmed bool
		wantUA    string
	}{
		{"newer billing version", "x-anthropic-billing-header: cc_version=2.1.322.245; cc_entrypoint=t3-code; cch=e9cc0;", true, incomingUA},
		{"older version on same credential", "x-anthropic-billing-header: cc_version=2.1.318.abc; cc_entrypoint=t3-code;", true, incomingUA},
		{"version without fingerprint", "x-anthropic-billing-header: cc_version=2.2.5; cc_entrypoint=t3-code;", true, incomingUA},
		{"unconfirmed caller keeps baseline entrypoint", "x-anthropic-billing-header: cc_version=2.1.322.245; cc_entrypoint=cli;", false, "claude-cli/2.1.280 (external, cli)"},
		{"missing billing", "", true, incomingUA},
		{"unrelated text", "cc_version=2.1.322.245;", true, incomingUA},
		{"malformed version", "x-anthropic-billing-header: cc_version=2.1.322.bad-version;", true, incomingUA},
		{"partial version", "x-anthropic-billing-header: cc_version=2.1;", true, incomingUA},
	}
	for _, tt := range tests {
		for _, prefix := range []string{"claude-cli", "CLAUDE-CLI"} {
			t.Run(tt.name+"/"+prefix, func(t *testing.T) {
				resetClaudeDeviceProfileCache()
				incoming := http.Header{
					"User-Agent":                  {strings.Replace(incomingUA, "claude-cli", prefix, 1)},
					"X-Stainless-Package-Version": {"0.128.0"},
					"X-Stainless-Runtime-Version": {"v26.12.1"},
					"X-Stainless-Os":              {"Linux"},
					"X-Stainless-Arch":            {"x64"},
				}
				body := []byte(fmt.Sprintf(`{"model":"claude-opus-5-5","system":[{"type":"text","text":%q}],"messages":[{"role":"user","content":"hello"}]}`, tt.billing))
				original := bytes.Clone(body)
				req := newClaudeHeaderTestRequest(t, incoming)
				if err := applyClaudeHeaders(req, auth, "sk-ant-oat-stabilized-billing", false, nil, body, cfg, incoming, tt.confirmed); err != nil {
					t.Fatal(err)
				}
				wantUA, wantSDK, wantNode := tt.wantUA, "0.112.1", "v26.3.0"
				if tt.confirmed {
					wantUA = strings.Replace(wantUA, "claude-cli", prefix, 1)
					wantSDK, wantNode = "0.128.0", "v26.12.1"
				}
				assertClaudeFingerprint(t, req.Header, wantUA, wantSDK, wantNode, "MacOS", "arm64")
				if !bytes.Equal(body, original) {
					t.Fatal("header construction changed the final payload")
				}
			})
		}
	}
}

func TestClaudeExecutor_StabilizedBillingPrecedesPayloadRules(t *testing.T) {
	const originalBilling = "x-anthropic-billing-header: cc_version=2.1.322.245; cc_entrypoint=t3-code; cch=00000;"
	const overrideBilling = "x-anthropic-billing-header: cc_version=2.1.323.abc; cc_entrypoint=t3-code; cch=00000;"
	const userID = `{"device_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","account_uuid":"","session_id":"11111111-2222-4333-8444-555555555555"}`
	for _, path := range []string{"execute", "stream", "count_tokens"} {
		for _, rule := range []string{"original", "override", "filter"} {
			t.Run(path+"/"+rule, func(t *testing.T) {
				var seenBody []byte
				var seenHeaders http.Header
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var errRead error
					seenBody, errRead = io.ReadAll(r.Body)
					if errRead != nil {
						t.Error(errRead)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					seenHeaders = r.Header.Clone()
					w.Header().Set("Content-Type", "application/json")
					if strings.HasSuffix(r.URL.Path, "/count_tokens") {
						_, _ = io.WriteString(w, `{"input_tokens":7}`)
					} else if ojson.GetBytes(seenBody, "stream").Bool() {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					} else {
						_, _ = io.WriteString(w, `{"id":"msg_billing","type":"message","model":"claude-opus-4-6","role":"assistant","content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
					}
				}))
				defer server.Close()

				stabilize := true
				cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
					StabilizeDeviceProfile: &stabilize,
					NativeEntrypoints:      []string{"t3-code"},
				}}
				wantVersion := "2.1.322"
				if rule == "override" {
					cfg.Payload.Override = []config.PayloadRule{{
						Models: []config.PayloadModelRule{{Name: "*"}},
						Params: map[string]any{"system.0.text": overrideBilling},
					}}
					wantVersion = "2.1.323"
				} else if rule == "filter" {
					cfg.Payload.Filter = []config.PayloadFilterRule{{
						Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"system"},
					}}
				}
				payload := []byte(fmt.Sprintf(`{"model":"claude-opus-4-6","max_tokens":16,"system":[{"type":"text","text":%q}],"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":%q}}`, originalBilling, userID))
				incoming := http.Header{
					"User-Agent":                  {"claude-cli/2.1.322 (external, t3-code, agent-sdk/0.3.316)"},
					"X-App":                       {"cli"},
					"Anthropic-Beta":              {"claude-code-20250219,oauth-2025-04-20"},
					"X-Stainless-Package-Version": {"0.128.0"},
					"X-Stainless-Runtime-Version": {"v26.12.1"},
					"X-Stainless-Os":              {"Linux"},
					"X-Stainless-Arch":            {"x64"},
				}
				auth := &cliproxyauth.Auth{ID: "final-billing-" + path + rule, Metadata: claudeOAuthTestMetadata(), Attributes: map[string]string{
					"api_key": "sk-ant-oat-final-billing",
				}}
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					upstream := r.Clone(r.Context())
					upstream.URL.Scheme = "http"
					upstream.URL.Host = server.Listener.Addr().String()
					upstream.Host = ""
					return http.DefaultTransport.RoundTrip(upstream)
				})))
				ex := NewClaudeExecutor(cfg)
				req := cliproxyexecutor.Request{Model: "claude-opus-4-6", Payload: payload}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload, Headers: incoming}
				var errRequest error
				switch path {
				case "execute":
					_, errRequest = ex.Execute(ctx, auth, req, opts)
				case "stream":
					var result *cliproxyexecutor.StreamResult
					result, errRequest = ex.ExecuteStream(ctx, auth, req, opts)
					if errRequest == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					}
				case "count_tokens":
					_, errRequest = ex.CountTokens(ctx, auth, req, opts)
				}
				if rule == "override" {
					var status interface{ StatusCode() int }
					if !errors.As(errRequest, &status) || status.StatusCode() != http.StatusBadRequest {
						t.Fatalf("contradictory final billing override error = %v, want400", errRequest)
					}
					if seenHeaders != nil {
						t.Fatal("contradictory profile reached upstream")
					}
					return
				}
				if errRequest != nil {
					t.Fatal(errRequest)
				}
				if seenHeaders == nil {
					t.Fatal("request did not reach the upstream server")
				}
				wantUA := "claude-cli/" + wantVersion + " (external, t3-code, agent-sdk/0.3.316)"
				if rule == "filter" || path == "count_tokens" && rule == "original" {
					if ojson.GetBytes(seenBody, "system").Exists() {
						t.Fatal("header construction restored the filtered system")
					}
				} else if billing := ojson.GetBytes(seenBody, "system.0.text").String(); !strings.Contains(billing, "cc_version="+wantVersion+".") {
					t.Fatalf("upstream billing = %q, want version %s", billing, wantVersion)
				}
				assertClaudeFingerprint(t, seenHeaders, wantUA, "0.128.0", "v26.12.1", "MacOS", "arm64")
				if path != "count_tokens" {
					profile, errProfile := helps.ResolveClaudeDeviceProfileRequired(t.Context(), auth, "sk-ant-oat-final-billing", incoming, cfg)
					if errProfile != nil {
						t.Fatal(errProfile)
					}
					assertClaudeCredentialIdentity(t, seenBody, seenHeaders, []string{profile.DeviceID}, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
				}
			})
		}
	}
}
