package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudecapture"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func testClaudeOAuthSafeguardProfile() *claudecapture.Profile {
	return &claudecapture.Profile{
		SchemaVersion: 1,
		ClaudeVersion: "2.1.258",
		CapturedAt:    time.Now(),
		Headers: map[string]string{
			"User-Agent":                  "claude-cli/2.1.258 (external, cli)",
			"Anthropic-Version":           "2023-06-01",
			"Anthropic-Beta":              "claude-code-20250219,oauth-2025-04-20",
			"X-Stainless-Lang":            "js",
			"X-Stainless-Package-Version": "0.112.1",
			"X-Stainless-OS":              "MacOS",
			"X-Stainless-Arch":            "arm64",
			"X-Stainless-Runtime":         "node",
			"X-Stainless-Runtime-Version": "v26.3.0",
		},
		Variants: []claudecapture.Variant{{Endpoint: "messages", Beta: "claude-code-20250219,oauth-2025-04-20", BodyKeys: []string{"messages", "model"}}},
	}
}

func testClaudeOAuthSafeguardMatchingProfile(t *testing.T) *claudecapture.Profile {
	t.Helper()
	profile := testClaudeOAuthSafeguardProfile()
	profile.Variants = nil
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatal(errRead)
		}
		var fields map[string]json.RawMessage
		if errJSON := json.Unmarshal(body, &fields); errJSON != nil {
			t.Fatal(errJSON)
		}
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		endpoint := "messages"
		if strings.HasSuffix(req.URL.Path, "/count_tokens") {
			endpoint = "count_tokens"
		}
		beta := ""
		for name, values := range req.Header {
			if strings.EqualFold(name, "Anthropic-Beta") && len(values) > 0 {
				beta = values[0]
			}
		}
		profile.Variants = append(profile.Variants, claudecapture.Variant{Endpoint: endpoint, Stream: gjson.GetBytes(body, "stream").Bool(), Beta: beta, BodyKeys: keys})
		if endpoint == "count_tokens" {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"input_tokens":7}`)), Request: req}, nil
		}
		if gjson.GetBytes(body, "stream").Bool() {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")), Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message","model":"claude-opus-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)), Request: req}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-ant-oat-guard-test"}, Metadata: claudeOAuthTestMetadata()}
	req, opts := testClaudeOAuthSafeguardRequest()
	if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
		t.Fatal(err)
	}
	stream, err := executor.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
	countPayload := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hello"}]}`)
	req.Payload, opts.OriginalRequest = countPayload, countPayload
	if _, err := executor.CountTokens(ctx, auth, req, opts); err != nil {
		t.Fatal(err)
	}
	return profile
}

func testClaudeOAuthSafeguardRequest() (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	const userID = `{"device_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","account_uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","session_id":"11111111-2222-4333-8444-555555555555"}`
	payload := []byte(`{"model":"claude-opus-4-6","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":` + fmt.Sprintf("%q", userID) + `}}`)
	headers := make(http.Header)
	for name, value := range testClaudeOAuthSafeguardProfile().Headers {
		if name != "Anthropic-Beta" {
			headers.Set(name, value)
		}
	}
	headers.Set("X-App", "cli")
	headers.Set("Anthropic-Beta", "claude-code-20250219,interleaved-thinking-2025-05-14")
	headers.Set("X-Claude-Code-Session-Id", "11111111-2222-4333-8444-555555555555")
	return cliproxyexecutor.Request{Model: "claude-opus-4-6", Payload: payload}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: payload,
		Headers:         headers,
	}
}

func TestClaudeOAuthSafeguardScopeAndCaptureFailure(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{OAuthSafeguard: true}})
	loads := 0
	executor.oauthSafeguardLoader = func() (*claudecapture.Profile, error) {
		loads++
		return nil, errors.New("no reference")
	}
	for _, test := range []struct {
		baseURL string
		key     string
		guarded bool
	}{
		{"https://api.anthropic.com", "sk-ant-oat-real", true},
		{"https://api.anthropic.com:443", "sk-ant-oat-real", true},
		{"https://api.anthropic.com", "sk-ant-api03-key", false},
		{"https://gateway.example", "sk-ant-oat-real", false},
		{"http://api.anthropic.com", "sk-ant-oat-real", false},
		{"https://api.anthropic.com.evil.example", "sk-ant-oat-real", false},
	} {
		_, guard, err := executor.loadClaudeOAuthSafeguard(test.baseURL, test.key)
		if test.guarded {
			if guard != nil || err == nil {
				t.Fatalf("%s %s: guard = %v, error = %v; want fail closed", test.baseURL, test.key, guard, err)
			}
			var scoped interface{ IsRequestScoped() bool }
			if !errors.As(err, &scoped) || !scoped.IsRequestScoped() {
				t.Fatalf("%s: error = %v, want request-scoped", test.baseURL, err)
			}
		} else if guard != nil || err != nil {
			t.Fatalf("%s %s: guard = %v, error = %v; want unguarded", test.baseURL, test.key, guard, err)
		}
	}
	if loads != 2 {
		t.Fatalf("capture loads = %d, want 2", loads)
	}
}

func TestClaudeOAuthSafeguardSkipsPreparatoryProfileLookup(t *testing.T) {
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-ant-oat-guard-test"}}
	if !NewClaudeExecutor(&config.Config{}).ShouldPrepareRequestAuth(auth) {
		t.Fatal("unprotected OAuth auth should prepare missing profile metadata")
	}
	guarded := NewClaudeExecutor(&config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{OAuthSafeguard: true}})
	if guarded.ShouldPrepareRequestAuth(auth) {
		t.Fatal("guarded OAuth auth attempted profile lookup before client validation")
	}
	auth.Attributes["base_url"] = "https://gateway.example"
	if !guarded.ShouldPrepareRequestAuth(auth) {
		t.Fatal("custom gateway OAuth auth should retain normal profile preparation")
	}
}

func TestClaudeOAuthSafeguardRejectsForeignClientBeforeUpstream(t *testing.T) {
	attempts := 0
	profiles := 0
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		return nil, errors.New("unexpected upstream request")
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	executor := NewClaudeExecutor(&config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{OAuthSafeguard: true}})
	executor.oauthSafeguardLoader = func() (*claudecapture.Profile, error) { return testClaudeOAuthSafeguardProfile(), nil }
	executor.oauthProfileFetcher = func(context.Context, *cliproxyauth.Auth, string) (*claudeauth.OAuthProfile, error) {
		profiles++
		return nil, errors.New("unexpected profile lookup")
	}
	metadata := claudeOAuthTestMetadata()
	delete(metadata, "account_uuid")
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-ant-oat-guard-test"}, Metadata: metadata}
	req, opts := testClaudeOAuthSafeguardRequest()
	opts.SourceFormat = sdktranslator.FromString("openai")
	if _, err := executor.Execute(ctx, auth, req, opts); err == nil || !strings.Contains(err.Error(), "native Claude") {
		t.Fatalf("Execute foreign client error = %v", err)
	}
	if _, err := executor.ExecuteStream(ctx, auth, req, opts); err == nil || !strings.Contains(err.Error(), "native Claude") {
		t.Fatalf("ExecuteStream foreign client error = %v", err)
	}
	if _, err := executor.CountTokens(ctx, auth, req, opts); err == nil || !strings.Contains(err.Error(), "native Claude") {
		t.Fatalf("CountTokens foreign client error = %v", err)
	}
	if attempts != 0 {
		t.Fatalf("upstream attempts = %d, want 0", attempts)
	}
	if profiles != 0 {
		t.Fatalf("OAuth profile lookups = %d, want 0", profiles)
	}
}

func TestClaudeOAuthSafeguardPreparesMissingIdentityAfterNativeCheck(t *testing.T) {
	for _, mode := range []string{"messages", "stream", "count_tokens"} {
		t.Run(mode, func(t *testing.T) {
			profile := testClaudeOAuthSafeguardMatchingProfile(t)
			var events []string
			transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				events = append(events, "upstream")
				if mode == "count_tokens" {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"input_tokens":7}`)), Request: req}, nil
				}
				if mode == "stream" {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")), Request: req}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message","model":"claude-opus-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)), Request: req}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
			executor := NewClaudeExecutor(&config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{OAuthSafeguard: true}})
			executor.oauthSafeguardLoader = func() (*claudecapture.Profile, error) { return profile, nil }
			executor.oauthProfileFetcher = func(_ context.Context, _ *cliproxyauth.Auth, token string) (*claudeauth.OAuthProfile, error) {
				if token != "sk-ant-oat-guard-test" {
					t.Fatalf("profile token = %q", token)
				}
				events = append(events, "profile")
				resolved := &claudeauth.OAuthProfile{}
				resolved.Account.UUID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
				return resolved, nil
			}
			metadata := claudeOAuthTestMetadata()
			delete(metadata, "account_uuid")
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-ant-oat-guard-test"}, Metadata: metadata}
			req, opts := testClaudeOAuthSafeguardRequest()
			if mode == "count_tokens" {
				countPayload := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hello"}]}`)
				req.Payload, opts.OriginalRequest = countPayload, countPayload
			}
			switch mode {
			case "messages":
				_, err := executor.Execute(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
			case "stream":
				stream, err := executor.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			case "count_tokens":
				_, err := executor.CountTokens(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
			}
			if strings.Join(events, ",") != "profile,upstream" {
				t.Fatalf("events = %v, want profile then upstream", events)
			}
			if _, found := auth.Metadata["account_uuid"]; found {
				t.Fatal("shared credential metadata mutated")
			}
		})
	}
}

func TestClaudeOAuthSafeguardAllowsMatchingNativeRequest(t *testing.T) {
	attempts := 0
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if req.URL.Host != "api.anthropic.com" {
			t.Fatalf("upstream host = %q", req.URL.Host)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message","model":"claude-opus-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)), Request: req}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	executor := NewClaudeExecutor(&config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{OAuthSafeguard: true}})
	profile := testClaudeOAuthSafeguardMatchingProfile(t)
	executor.oauthSafeguardLoader = func() (*claudecapture.Profile, error) { return profile, nil }
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-ant-oat-guard-test"}, Metadata: claudeOAuthTestMetadata()}
	req, opts := testClaudeOAuthSafeguardRequest()
	if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
		t.Fatalf("Execute native request: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("upstream attempts = %d, want 1", attempts)
	}
}

func TestClaudeOAuthSafeguardAllowsNativeStreamAndTokenCount(t *testing.T) {
	attempts := 0
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if strings.HasSuffix(req.URL.Path, "/count_tokens") {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"input_tokens":7}`)), Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")), Request: req}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	executor := NewClaudeExecutor(&config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{OAuthSafeguard: true}})
	profile := testClaudeOAuthSafeguardMatchingProfile(t)
	executor.oauthSafeguardLoader = func() (*claudecapture.Profile, error) { return profile, nil }
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-ant-oat-guard-test"}, Metadata: claudeOAuthTestMetadata()}
	req, opts := testClaudeOAuthSafeguardRequest()
	stream, errStream := executor.ExecuteStream(ctx, auth, req, opts)
	if errStream != nil {
		t.Fatalf("ExecuteStream native request: %v", errStream)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk: %v", chunk.Err)
		}
	}
	countPayload := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hello"}]}`)
	req.Payload = countPayload
	opts.OriginalRequest = countPayload
	if _, errCount := executor.CountTokens(ctx, auth, req, opts); errCount != nil {
		t.Fatalf("CountTokens native request: %v", errCount)
	}
	if attempts != 2 {
		t.Fatalf("upstream attempts = %d, want 2", attempts)
	}
}

func TestClaudeOAuthSafeguardRejectsOutboundHeaderOverrideBeforeUpstream(t *testing.T) {
	attempts := 0
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		return nil, errors.New("unexpected upstream request")
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	executor := NewClaudeExecutor(&config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{OAuthSafeguard: true}})
	executor.oauthSafeguardLoader = func() (*claudecapture.Profile, error) { return testClaudeOAuthSafeguardProfile(), nil }
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":           "sk-ant-oat-guard-test",
			"header:User-Agent": "another-client/1.0",
		},
		Metadata: claudeOAuthTestMetadata(),
	}
	req, opts := testClaudeOAuthSafeguardRequest()
	if _, err := executor.Execute(ctx, auth, req, opts); err == nil || !strings.Contains(err.Error(), "outbound request") {
		t.Fatalf("Execute override error = %v", err)
	}
	if attempts != 0 {
		t.Fatalf("upstream attempts = %d, want 0", attempts)
	}
}
