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
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
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
	if effective, _, err := executor.loadClaudeOAuthSafeguard("https://api.anthropic.com", "sk-ant-api03-key"); err != nil {
		t.Fatal(err)
	} else if effective == nil || effective.ClaudeHeaderDefaults.OAuthSafeguard {
		t.Fatal("unguarded API-key detection retained the OAuth safeguard relaxation")
	} else {
		req, opts := testClaudeOAuthSafeguardRequest()
		opts.Headers.Set("User-Agent", "claude-cli/999.0.0 (external, cli)")
		if detection := helps.DetectClaudeCodeRequest(opts.Headers, req.Payload, false, effective); detection.Confirmed {
			t.Fatal("unguarded API-key request accepted an unmeasured Claude CLI version")
		}
	}
	if loads != 2 {
		t.Fatalf("capture loads = %d, want 2", loads)
	}
}

func TestClaudeOAuthSafeguardPreservesConfiguredDefaults(t *testing.T) {
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
		OAuthSafeguard: true,
		UserAgent:      "claude-cli/2.1.70 (external, cli)",
		PackageVersion: "0.80.0",
		RuntimeVersion: "v24.5.0",
		OS:             "Linux",
		Arch:           "x64",
	}}
	executor := NewClaudeExecutor(cfg)
	executor.oauthSafeguardLoader = func() (*claudecapture.Profile, error) {
		profile := testClaudeOAuthSafeguardProfile()
		delete(profile.Headers, "X-Stainless-Package-Version")
		delete(profile.Headers, "X-Stainless-Runtime-Version")
		return profile, nil
	}
	effective, guard, err := executor.loadClaudeOAuthSafeguard("https://api.anthropic.com", "sk-ant-oat-guard-test")
	if err != nil {
		t.Fatal(err)
	}
	if guard == nil {
		t.Fatal("safeguard = nil, want loaded guard")
	}
	if effective != cfg {
		t.Fatal("effective config is a copy; want original config to preserve header defaults")
	}
	if got := effective.ClaudeHeaderDefaults; got != cfg.ClaudeHeaderDefaults {
		t.Fatalf("Claude header defaults changed: got %+v, want %+v", got, cfg.ClaudeHeaderDefaults)
	}
}

func TestClaudeOAuthSafeguardAcceptsCurrentNativeSoftwareVersions(t *testing.T) {
	profile := testClaudeOAuthSafeguardProfile()
	headers := make(http.Header)
	for name, value := range profile.Headers {
		headers.Set(name, value)
	}
	headers.Set("User-Agent", "claude-cli/9.8.7 (external, cli)")
	headers.Set("X-Stainless-Package-Version", "0.999.0")
	headers.Set("X-Stainless-Runtime-Version", "v99.1.2")
	guard := &claudeOAuthSafeguard{profile: profile}
	detection := helps.ClaudeCodeRequestDetection{Confirmed: true, NativeClient: true}
	if err := guard.checkIncoming(sdktranslator.FormatClaude, headers, detection); err != nil {
		t.Fatalf("checkIncoming() rejected current native software versions: %v", err)
	}
}

func TestClaudeOAuthSafeguardForeignUserAgentErrorAndLog(t *testing.T) {
	const foreignUserAgent = "OpenAI/secret-test-token"
	previousLevel := log.GetLevel()
	log.SetLevel(log.WarnLevel)
	hook := logrustest.NewLocal(log.StandardLogger())
	t.Cleanup(func() {
		hook.Reset()
		log.SetLevel(previousLevel)
	})
	guard := &claudeOAuthSafeguard{profile: testClaudeOAuthSafeguardProfile()}
	headers := make(http.Header)
	headers.Set("User-Agent", foreignUserAgent)
	err := guard.checkIncoming(sdktranslator.FromString("openai"), headers, helps.ClaudeCodeRequestDetection{})
	if err == nil || !strings.Contains(err.Error(), "expects User-Agent to be 'claude-cli/X.X.X'") || !strings.Contains(err.Error(), "received '"+foreignUserAgent+"'.") {
		t.Fatalf("checkIncoming() error = %v, want expected format and received User-Agent", err)
	}
	entries := hook.AllEntries()
	if len(entries) != 1 {
		t.Fatalf("log entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Data["reason"] != "client User-Agent does not match claude-cli/X.X.X" {
		t.Fatalf("logged rejection reason = %v", entry.Data["reason"])
	}
	if strings.Contains(entry.Message, foreignUserAgent) || strings.Contains(fmt.Sprint(entry.Data), foreignUserAgent) {
		t.Fatalf("log leaked received User-Agent: %+v", entry)
	}
}

func TestClaudeOAuthSafeguardExplainsMissingNativeSignal(t *testing.T) {
	guard := &claudeOAuthSafeguard{profile: testClaudeOAuthSafeguardProfile()}
	headers := make(http.Header)
	headers.Set("User-Agent", "claude-cli/9.8.7 (external, cli)")
	for _, test := range []struct {
		name      string
		detection helps.ClaudeCodeRequestDetection
		want      string
	}{
		{"unsupported entrypoint", helps.ClaudeCodeRequestDetection{}, "supported Claude Code client entrypoint"},
		{"missing X-App", helps.ClaudeCodeRequestDetection{NativeClient: true}, "X-App to be 'cli'"},
		{"missing beta", helps.ClaudeCodeRequestDetection{NativeClient: true, XAppCLI: true}, "Claude Code beta or a supported native helper request"},
		{"missing metadata", helps.ClaudeCodeRequestDetection{NativeClient: true, XAppCLI: true, BetasPresent: true}, "valid Claude Code session metadata"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := guard.checkIncoming(sdktranslator.FormatClaude, headers, test.detection)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("checkIncoming() error = %v, want %q", err, test.want)
			}
		})
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

func TestClaudeOAuthSafeguardAllowsSDKEntrypoint(t *testing.T) {
	const sdkUserAgent = "claude-cli/9.8.7 (external, sdk)"
	attempts := 0
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if got := req.Header.Get("User-Agent"); got != sdkUserAgent {
			t.Errorf("outbound User-Agent = %q, want %q", got, sdkUserAgent)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message","model":"claude-opus-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)), Request: req}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	executor := NewClaudeExecutor(&config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{OAuthSafeguard: true}})
	profile := testClaudeOAuthSafeguardMatchingProfile(t)
	executor.oauthSafeguardLoader = func() (*claudecapture.Profile, error) { return profile, nil }
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-ant-oat-guard-test"}, Metadata: claudeOAuthTestMetadata()}
	req, opts := testClaudeOAuthSafeguardRequest()
	opts.Headers.Set("User-Agent", sdkUserAgent)
	if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
		t.Fatalf("Execute SDK request: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("upstream attempts = %d, want 1", attempts)
	}
	opts.Headers.Del("X-App")
	if _, err := executor.Execute(ctx, auth, req, opts); err == nil || !strings.Contains(err.Error(), "X-App") {
		t.Fatalf("Execute SDK request without X-App error = %v", err)
	}
	if attempts != 1 {
		t.Fatalf("upstream attempts after invalid SDK request = %d, want 1", attempts)
	}
}

func TestClaudeOAuthSafeguardAllowsDifferentClientVersionAndConfiguredVersions(t *testing.T) {
	profile := testClaudeOAuthSafeguardMatchingProfile(t)
	attempts := 0
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if got := req.Header.Get("User-Agent"); got != "claude-cli/9.8.7 (external, cli)" {
			t.Errorf("outbound User-Agent = %q", got)
		}
		if got := req.Header.Get("X-Stainless-Package-Version"); got != "0.80.0" {
			t.Errorf("outbound package version = %q", got)
		}
		if got := req.Header.Get("X-Stainless-Runtime-Version"); got != "v24.5.0" {
			t.Errorf("outbound runtime version = %q", got)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message","model":"claude-opus-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)), Request: req}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
		OAuthSafeguard: true,
		PackageVersion: "0.80.0",
		RuntimeVersion: "v24.5.0",
	}}
	executor := NewClaudeExecutor(cfg)
	executor.oauthSafeguardLoader = func() (*claudecapture.Profile, error) { return profile, nil }
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-ant-oat-guard-test"}, Metadata: claudeOAuthTestMetadata()}
	req, opts := testClaudeOAuthSafeguardRequest()
	opts.Headers.Set("User-Agent", "claude-cli/9.8.7 (external, cli)")
	opts.Headers.Set("X-Stainless-Package-Version", "0.99.0")
	opts.Headers.Set("X-Stainless-Runtime-Version", "v25.0.0")
	opts.Headers.Set("X-Stainless-OS", "Windows")
	opts.Headers.Set("X-Stainless-Arch", "x64")
	if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
		t.Fatalf("Execute native request with a different version and platform: %v", err)
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
