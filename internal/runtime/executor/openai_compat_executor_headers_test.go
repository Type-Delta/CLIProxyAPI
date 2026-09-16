package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestOpenAICompatExecutorForwardsClientHeaders(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
	}{
		{name: "execute"},
		{name: "stream", stream: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var gotHeaders http.Header
			var gotHost string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHeaders = r.Header.Clone()
				gotHost = r.Host
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
			}))
			defer server.Close()

			executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
			auth := &cliproxyauth.Auth{
				Provider: "openai-compatibility",
				Attributes: map[string]string{
					"base_url":             server.URL,
					"api_key":              "provider-key",
					"header:X-Static":      "configured-value",
					"header:X-From-Client": "$X-Client-Source",
				},
			}
			req := cliproxyexecutor.Request{
				Model:   "gpt-4o",
				Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			}
			opts := cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FormatOpenAI,
				Stream:       test.stream,
				Headers: http.Header{
					"session-id":          {"codex-session"},
					"X-Session-Id":        {"generic-session"},
					"X-Session-Affinity":  {"ses_opencode"},
					"x-opencode-session":  {"opencode-session"},
					"User-Agent":          {"codex-cli/0.154.0"},
					"X-Arbitrary":         {"arbitrary-value"},
					"X-Multi":             {"first", "second"},
					"X-Client-Source":     {"dynamic-value"},
					"X-Static":            {"downstream-value"},
					"Authorization":       {"Bearer downstream-cpa-key"},
					"X-Api-Key":           {"downstream-cpa-key"},
					"Api-Key":             {"downstream-cpa-key"},
					"Proxy-Authorization": {"Basic downstream-cpa-key"},
					"Proxy-Authenticate":  {"Basic realm=downstream"},
					"Proxy-Connection":    {"keep-alive"},
					"Connection":          {"X-Connection-Scoped, keep-alive"},
					"X-Connection-Scoped": {"must-not-forward"},
					"Keep-Alive":          {"timeout=5"},
					"Te":                  {"trailers"},
					"Trailer":             {"X-Trailer"},
					"Transfer-Encoding":   {"chunked"},
					"Upgrade":             {"websocket"},
					"Host":                {"downstream.example"},
					"Content-Length":      {"999"},
					"Content-Encoding":    {"gzip"},
					"Content-Type":        {"application/octet-stream"},
					"Accept-Encoding":     {"br"},
				},
			}

			if test.stream {
				result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatalf("ExecuteStream() error = %v", err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("ExecuteStream() chunk error = %v", chunk.Err)
					}
				}
			} else if _, err := executor.Execute(context.Background(), auth, req, opts); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}

			for name, want := range map[string]string{
				"session-id":         "codex-session",
				"X-Session-Id":       "generic-session",
				"X-Session-Affinity": "ses_opencode",
				"x-opencode-session": "opencode-session",
				"User-Agent":         "codex-cli/0.154.0",
				"X-Arbitrary":        "arbitrary-value",
				"X-Static":           "configured-value",
				"X-From-Client":      "dynamic-value",
			} {
				if got := gotHeaders.Get(name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
			if got := gotHeaders.Values("X-Multi"); len(got) != 2 || got[0] != "first" || got[1] != "second" {
				t.Errorf("X-Multi = %#v, want [first second]", got)
			}
			if got := gotHeaders.Get("Authorization"); got != "Bearer provider-key" {
				t.Errorf("Authorization = %q, want provider credential", got)
			}
			if got := gotHeaders.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if gotHost == "downstream.example" {
				t.Error("downstream Host header was forwarded")
			}
			for _, name := range []string{
				"X-Api-Key",
				"Api-Key",
				"Proxy-Authorization",
				"Proxy-Authenticate",
				"Proxy-Connection",
				"Connection",
				"X-Connection-Scoped",
				"Keep-Alive",
				"Te",
				"Trailer",
				"Transfer-Encoding",
				"Upgrade",
				"Content-Encoding",
			} {
				if got := gotHeaders.Get(name); got != "" {
					t.Errorf("%s = %q, want omitted", name, got)
				}
			}
			if got := gotHeaders.Get("Accept-Encoding"); got == "br" {
				t.Error("downstream Accept-Encoding was forwarded")
			}
			if got := gotHeaders.Get("Content-Length"); got == "999" {
				t.Error("downstream Content-Length was forwarded")
			}
		})
	}
}

func TestOpenAICompatRequestHeadersDropDownstreamCredentialWithoutProviderKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/chat/completions", nil)
	clientHeaders := http.Header{
		"Authorization": {"Bearer downstream-cpa-key"},
		"X-Api-Key":     {"downstream-cpa-key"},
		"Api-Key":       {"downstream-cpa-key"},
	}

	applyOpenAICompatRequestHeaders(req, "", "application/json", nil, clientHeaders)

	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want omitted", got)
	}
	if got := req.Header.Get("X-Api-Key"); got != "" {
		t.Errorf("X-Api-Key = %q, want omitted", got)
	}
	if got := req.Header.Get("Api-Key"); got != "" {
		t.Errorf("Api-Key = %q, want omitted", got)
	}
}
