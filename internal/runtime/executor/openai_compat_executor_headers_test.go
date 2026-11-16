package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOpenAICompatExecutorForwardsClientHeaders(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
		source sdktranslator.Format
	}{
		{name: "execute", source: sdktranslator.FormatOpenAI},
		{name: "stream", stream: true, source: sdktranslator.FormatOpenAI},
		{name: "responses-execute", source: sdktranslator.FormatOpenAIResponse},
		{name: "responses-stream", stream: true, source: sdktranslator.FormatOpenAIResponse},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var gotHeaders http.Header
			var gotHost string
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHeaders = r.Header.Clone()
				gotHost = r.Host
				var errRead error
				gotBody, errRead = io.ReadAll(r.Body)
				if errRead != nil {
					t.Error(errRead)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
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
					"header:X-Real-IP":     "configured-ip",
				},
			}
			req := cliproxyexecutor.Request{
				Model:   "gpt-4o",
				Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			}
			if test.source == sdktranslator.FormatOpenAIResponse {
				req.Payload = []byte(`{"input":"hi","tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"web_search"}}`)
			}
			opts := cliproxyexecutor.Options{
				SourceFormat: test.source,
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
					"X-Goog-Api-Key":      {"downstream-cpa-key"},
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
					"Forwarded":           {"for=192.0.2.10;proto=https"},
					"X-Forwarded-For":     {"192.0.2.10"},
					"x-forwarded-custom":  {"proxy-value"},
					"X-Real-Ip":           {"192.0.2.10"},
					"True-Client-Ip":      {"192.0.2.10"},
					"X-Client-Ip":         {"192.0.2.10"},
					"Cf-Connecting-Ip":    {"192.0.2.10"},
					"Cf-Connecting-Ipv6":  {"2001:db8::1"},
					"Cf-Ipcountry":        {"ZZ"},
					"Cf-Ray":              {"test-ray"},
					"cf-visitor":          {`{"scheme":"https"}`},
					"Cdn-Loop":            {"cloudflare"},
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
			if test.source == sdktranslator.FormatOpenAIResponse {
				if ojson.GetBytes(gotBody, "tools.0.function.name").String() != "web_search" || ojson.GetBytes(gotBody, "tool_choice.function.name").String() != "web_search" {
					t.Fatalf("caller-owned web_search function lost during translation: %s", gotBody)
				}
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
				"X-Real-IP":          "configured-ip",
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
				"X-Goog-Api-Key",
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
				"Forwarded",
				"X-Forwarded-For",
				"X-Forwarded-Custom",
				"True-Client-IP",
				"X-Client-IP",
				"Cf-Connecting-IP",
				"Cf-Connecting-IPv6",
				"Cf-IPCountry",
				"Cf-Ray",
				"Cf-Visitor",
				"Cdn-Loop",
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
		"Authorization":  {"Bearer downstream-cpa-key"},
		"X-Api-Key":      {"downstream-cpa-key"},
		"X-Goog-Api-Key": {"downstream-cpa-key"},
		"Api-Key":        {"downstream-cpa-key"},
		"X-Real-Ip":      {"192.0.2.10"},
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
	if got := req.Header.Get("X-Goog-Api-Key"); got != "" {
		t.Errorf("X-Goog-Api-Key = %q, want omitted", got)
	}
	if got := req.Header.Get("X-Real-IP"); got != "" {
		t.Errorf("X-Real-IP = %q, want omitted", got)
	}
}
