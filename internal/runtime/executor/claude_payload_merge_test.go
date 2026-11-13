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
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	tr "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestClaudeFallbackPayloadBarrierMergedPaths(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, model, rewrite, caller, configured, want string
			enabled, filter                                bool
		}{
			{name: "default off Fable", model: "claude-fable-5-1"},
			{name: "default off Opus", model: "claude-opus-5-5"},
			{name: "opt in Fable", model: "claude-fable-5-1", enabled: true, want: "claude-opus-5"},
			{name: "opt in Opus", model: "claude-opus-5-5", enabled: true, want: "claude-opus-4-8"},
			{name: "default off rewrite to Fable", model: "claude-sonnet-5", rewrite: "claude-fable-5-1"},
			{name: "opt in rewrite to Fable keeps prepared body", model: "claude-sonnet-5", rewrite: "claude-fable-5-1", enabled: true},
			{name: "opt in rewrite from Fable keeps prepared body", model: "claude-fable-5-1", rewrite: "claude-sonnet-5", enabled: true, want: "claude-opus-5"},
			{name: "opt in Opus rewrite to Fable", model: "claude-opus-5-5", rewrite: "claude-fable-5-1", enabled: true, want: "claude-opus-4-8"},
			{name: "caller fallback preserved while off", model: "claude-fable-5-1", caller: "caller-choice", want: "caller-choice"},
			{name: "caller fallback preserved across rewrite", model: "claude-opus-5-5", rewrite: "claude-fable-5-1", caller: "caller-choice", enabled: true, want: "caller-choice"},
			{name: "payload fallback while off", model: "claude-sonnet-5", rewrite: "claude-fable-5-1", configured: "payload-choice", want: "payload-choice"},
			{name: "payload fallback replaces opt in", model: "claude-fable-5-1", configured: "payload-choice", enabled: true, want: "payload-choice"},
			{name: "filter wins across rewrite", model: "claude-opus-5-5", rewrite: "claude-fable-5-1", enabled: true, filter: true},
		} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				cfg := &config.Config{Claude: config.ClaudeConfig{ServerSideFallback: tc.enabled}}
				params := map[string]any{}
				if tc.rewrite != "" {
					params["model"] = tc.rewrite
				}
				if tc.configured != "" {
					params["fallbacks"] = []any{map[string]any{"model": tc.configured}}
				}
				if len(params) > 0 {
					cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: params}}
				}
				if tc.filter {
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"fallbacks"}}}
				}
				doc := ojson.NewDocString(`{"messages":[{"role":"user","content":"hello"}]}`)
				_ = doc.Set("model", tc.model)
				if tc.caller != "" {
					_ = doc.Set("fallbacks", []any{map[string]any{"model": tc.caller}})
				}
				var seen []byte
				var headers http.Header
				transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					seen, _ = io.ReadAll(r.Body)
					headers = r.Header.Clone()
					contentType, response := "application/json", claudeThreadTestMessage("msg_barrier")
					if stream {
						contentType, response = "text/event-stream", claudeThreadTestSSE("msg_barrier")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
				})
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
				credential := &auth.Auth{ID: t.Name(), Metadata: claudeOAuthTestMetadata(), Attributes: map[string]string{"api_key": "sk-ant-oat-barrier", "cloak_mode": "always"}}
				engine := NewClaudeExecutor(cfg)
				req := ex.Request{Model: tc.model, Payload: doc.Bytes()}
				opts := ex.Options{SourceFormat: tr.FormatClaude, Stream: stream}
				before := helps.PayloadConfigEvaluationCount()
				if stream {
					result, err := engine.ExecuteStream(ctx, credential, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := engine.Execute(ctx, credential, req, opts); err != nil {
					t.Fatal(err)
				}
				if count := helps.PayloadConfigEvaluationCount() - before; count != 1 {
					t.Fatalf("payload evaluated %d times", count)
				}
				if got := ojson.GetBytes(seen, "fallbacks.0.model").String(); got != tc.want {
					t.Fatalf("fallback=%q want=%q body=%s", got, tc.want, seen)
				}
				wantModel := tc.model
				if tc.rewrite != "" {
					wantModel = tc.rewrite
				}
				if got := ojson.GetBytes(seen, "model").String(); got != wantModel {
					t.Fatalf("model=%q want=%q", got, wantModel)
				}
				if got := strings.Contains(helps.HeaderValueCaseInsensitive(headers, "Anthropic-Beta"), claudeServerSideFallbackBeta); got != (tc.want != "") {
					t.Fatalf("fallback beta present=%v fallback=%q", got, tc.want)
				}
			})
		}
	}
}
