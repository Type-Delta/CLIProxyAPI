package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/antigravitycapture"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

func TestAntigravityOAuthSafeguardGeminiStreamUsesCapturedEnvelope(t *testing.T) {
	const ua = "antigravity/cli/1.5.15 (aidev_client; os_type=windows; arch=amd64; cl=1027820220; auth_method=consumer)"
	const body = `{"contents":[{"role":"user","parts":[{"text":"Describe the image"},{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]}],"tools":[{"functionDeclarations":[{"name":"write_file","parameters":{"type":"object","properties":{"content":{"type":"string"}}}}]}]}`
	const response = "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"write_file\",\"args\":{\"content\":\"example\"}}}]},\"finishReason\":\"STOP\"}]}}\n\n"
	cfg := &config.Config{}
	cfg.Antigravity.OAuthSafeguard = true
	executor := NewAntigravityExecutor(cfg)
	executor.referenceLoader = func() (*antigravitycapture.Profile, error) {
		return &antigravitycapture.Profile{Version: "1.5.15", UserAgent: ua, ContentType: "application/json", EnvelopeUserAgent: "antigravity"}, nil
	}
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "antigravity", Metadata: map[string]any{
		"access_token": "fixture-token", "project_id": "fixture-project", "expired": time.Now().Add(time.Hour).Format(time.RFC3339),
	}}
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", roundTripperFunc(func(upstream *http.Request) (*http.Response, error) {
		wire, err := io.ReadAll(upstream.Body)
		if err != nil {
			t.Fatal(err)
		}
		if upstream.URL.Path != antigravityStreamPath || upstream.Header.Get("User-Agent") != ua ||
			upstream.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Fatalf("unexpected request URL or reference headers: %s %s", upstream.URL.Path, upstream.Header.Get("User-Agent"))
		}
		doc := ojson.NewDoc(wire)
		if doc.Get("project").Str != "fixture-project" || doc.Get("requestType").Str != "agent" ||
			doc.Get("request.contents.0.parts.1.inlineData.mimeType").Str != "image/png" ||
			doc.Get("request.tools.0.functionDeclarations.0.name").Str != "write_file" {
			t.Fatalf("native request lost project, image, or tool: %s", wire)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(response))}, nil
	}))
	req := cliproxyexecutor.Request{Model: "gemini-test", Payload: []byte(body)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Stream: true, Headers: http.Header{
		"User-Agent": {"google-genai-sdk/1.79.1"}, "Content-Type": {"application/json"},
	}}
	stream, err := executor.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var result strings.Builder
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		result.Write(chunk.Payload)
	}
	if !strings.Contains(result.String(), `"functionCall"`) || !strings.Contains(result.String(), `"write_file"`) {
		t.Fatalf("Gemini client did not receive its tool call: %s", result.String())
	}
}

func TestAntigravityOAuthSafeguard(t *testing.T) {
	const ua = "antigravity/cli/1.5.15 (aidev_client; os_type=windows; arch=amd64; cl=1027820220; auth_method=consumer)"
	const native = `{"model":"gemini-test","project":"client-project","userAgent":"antigravity","requestType":"agent","requestId":"agent-1","request":{"sessionId":"-1","contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`
	cfg := &config.Config{}
	cfg.Antigravity.OAuthSafeguard = true
	exec := NewAntigravityExecutor(cfg)
	loadReference := func() (*antigravitycapture.Profile, error) {
		return &antigravitycapture.Profile{Version: "1.5.15", UserAgent: ua, ContentType: "application/json", EnvelopeUserAgent: "antigravity"}, nil
	}
	exec.referenceLoader = loadReference
	auth := &cliproxyauth.Auth{Provider: "antigravity", Metadata: map[string]any{
		"access_token": "test-token", "project_id": "credential-project",
	}}
	req := cliproxyexecutor.Request{Model: "gemini-test", Payload: []byte(native)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, Headers: http.Header{
		"User-Agent": {ua}, "Content-Type": {"application/json"},
	}}

	t.Run("missing reference stops guarded inference", func(t *testing.T) {
		withoutProfile := NewAntigravityExecutor(cfg)
		withoutProfile.referenceLoader = func() (*antigravitycapture.Profile, error) { return nil, errors.New("missing reference") }
		_, err := withoutProfile.Execute(t.Context(), auth, req, opts)
		var unavailable antigravityReferenceUnavailableError
		if !errors.As(err, &unavailable) || unavailable.StatusCode() != http.StatusServiceUnavailable || !unavailable.IsCredentialScoped() {
			t.Fatalf("expected credential-scoped 503, got %v", err)
		}
	})

	t.Run("matching native profile", func(t *testing.T) {
		if err := exec.checkAntigravityOAuthIncoming(auth, req, opts, false); err != nil {
			t.Fatal(err)
		}
		httpReq, body, err := exec.buildRequestDoc(t.Context(), auth, "test-token", req.Model, ojson.NewDoc(req.Payload), true, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if got := ojson.GetBytes(body, "project").Str; got != "credential-project" || httpReq.URL.Path != antigravityStreamPath {
			t.Fatalf("outbound project=%q path=%s", got, httpReq.URL.Path)
		}
		if got := httpReq.Header.Get("User-Agent"); got != ua {
			t.Fatalf("outbound User-Agent = %q, want captured value", got)
		}
	})

	t.Run("custom incoming client identity", func(t *testing.T) {
		custom := *cfg
		if err := yaml.Unmarshal([]byte("oauth-safeguard-client-identities: [t3code, t3-code]"), &custom.Antigravity); err != nil {
			t.Fatal(err)
		}
		executor := NewAntigravityExecutor(&custom)
		executor.referenceLoader = loadReference
		client := opts
		client.Headers = opts.Headers.Clone()
		client.Headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0) t3code/0.0.15 Chrome/136.0.7103.93 Electron/36.3.2")
		if err := exec.checkAntigravityOAuthIncoming(auth, req, client, false); err == nil || !strings.Contains(err.Error(), "incoming User-Agent") {
			t.Fatalf("expected User-Agent rejection, got %v", err)
		}
		if err := executor.checkAntigravityOAuthIncoming(auth, req, client, false); err != nil {
			t.Fatalf("configured client rejected: %v", err)
		}
		translated := client
		translated.SourceFormat = sdktranslator.FormatOpenAI
		if err := executor.checkAntigravityOAuthIncoming(auth, req, translated, false); err == nil || !strings.Contains(err.Error(), "native Antigravity inference endpoint") {
			t.Fatalf("expected native endpoint rejection, got %v", err)
		}
		malformed := req
		malformed.Payload = []byte(`{"request":{"contents":[]}}`)
		if err := executor.checkAntigravityOAuthIncoming(auth, malformed, client, false); err == nil {
			t.Fatal("configured client bypassed envelope validation")
		}
		for _, rejectedUA := range []string{
			"Mozilla/5.0 (t3code/0.0.15)",
			"not-t3code/0.0.15",
			"t3code-foreign/0.0.15",
		} {
			client.Headers.Set("User-Agent", rejectedUA)
			if err := executor.checkAntigravityOAuthIncoming(auth, req, client, false); err == nil {
				t.Errorf("unconfigured User-Agent %q passed", rejectedUA)
			}
		}
		client.Headers.Set("User-Agent", "T3-Code/1.0.0-nightly")
		if err := executor.checkAntigravityOAuthIncoming(auth, req, client, false); err != nil {
			t.Fatalf("configured case-insensitive product rejected: %v", err)
		}
		client.Headers.Add("User-Agent", ua)
		if err := executor.checkAntigravityOAuthIncoming(auth, req, client, false); err == nil {
			t.Fatal("duplicate User-Agent headers passed")
		}
		outbound := auth.Clone()
		outbound.Attributes = map[string]string{"header:User-Agent": "t3code/0.0.15"}
		if _, _, err := executor.buildRequestDoc(t.Context(), outbound, "test-token", req.Model, ojson.NewDoc(req.Payload), true, "", ""); err == nil {
			t.Fatal("configured client identity altered the outbound User-Agent policy")
		}
	})

	t.Run("foreign client refused before transport", func(t *testing.T) {
		bad := opts
		bad.SourceFormat = sdktranslator.FormatOpenAI
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", roundTripperFunc(func(*http.Request) (*http.Response, error) {
			t.Error("foreign request used the transport")
			return nil, errors.New("unexpected transport")
		}))
		_, errExecute := exec.Execute(ctx, auth, req, bad)
		_, errStream := exec.ExecuteStream(ctx, auth, req, bad)
		_, errCount := exec.CountTokens(ctx, auth, req, bad)
		for _, err := range []error{errExecute, errStream, errCount} {
			var reject antigravityOAuthSafeguardError
			if !errors.As(err, &reject) || reject.StatusCode() != http.StatusForbidden || !reject.IsRequestScoped() {
				t.Fatalf("expected request-scoped 403, got %v", err)
			}
		}
	})

	t.Run("Gemini SDK is allowed by default with an explicit opt-out", func(t *testing.T) {
		custom := *cfg
		custom.Antigravity.OAuthSafeguardAllowGeminiSDK = new(false)
		executor := NewAntigravityExecutor(&custom)
		executor.referenceLoader = loadReference
		client := opts
		client.SourceFormat = sdktranslator.FormatGemini
		client.Headers = opts.Headers.Clone()
		client.Headers.Set("User-Agent", "google-genai-sdk/1.79.1 gl-go/go1.28-20261102-RC01")
		payload := req
		payload.Payload = []byte(`{"contents":[{"role":"user","parts":[{"text":"read this image"},{"inlineData":{"mimeType":"image/png","data":"dGVzdA=="}}]}]}`)
		if err := exec.checkAntigravityOAuthIncoming(auth, payload, client, false); err != nil {
			t.Fatalf("default Gemini SDK client rejected: %v", err)
		}
		nativeRoute := client
		nativeRoute.SourceFormat = sdktranslator.FormatAntigravity
		if err := exec.checkAntigravityOAuthIncoming(auth, req, nativeRoute, false); err == nil {
			t.Fatal("default Gemini SDK identity passed on the native route")
		}
		if err := executor.checkAntigravityOAuthIncoming(auth, payload, client, false); err == nil {
			t.Fatal("Gemini SDK passed after disabling its default identity")
		}
		custom.Antigravity.OAuthSafeguardClientIdentities = []string{"t3code"}
		client.Headers.Set("User-Agent", "t3code/1.0")
		if err := executor.checkAntigravityOAuthIncoming(auth, payload, client, false); err != nil {
			t.Fatalf("custom identity rejected after disabling the default: %v", err)
		}
		client.Headers.Set("User-Agent", ua)
		if err := executor.checkAntigravityOAuthIncoming(auth, payload, client, false); err == nil {
			t.Fatal("a native User-Agent alone opted into Gemini")
		}
		client.Headers.Set("User-Agent", "google-genai-sdk/1.79.1")
		if err := exec.checkAntigravityOAuthIncoming(auth, req, client, false); err == nil {
			t.Fatal("Gemini route accepted a non-Gemini body")
		}
		missingRef := NewAntigravityExecutor(cfg)
		missingRef.referenceLoader = func() (*antigravitycapture.Profile, error) { return nil, errors.New("unavailable") }
		var unavailable antigravityReferenceUnavailableError
		if err := missingRef.checkAntigravityOAuthIncoming(auth, payload, client, false); !errors.As(err, &unavailable) {
			t.Fatalf("configured Gemini bypassed the reference check: %v", err)
		}
	})

	t.Run("outbound authentication override refused", func(t *testing.T) {
		custom := auth.Clone()
		custom.Attributes = map[string]string{"header:Authorization": "Bearer different-token"}
		_, _, err := exec.buildRequestDoc(t.Context(), custom, "test-token", req.Model, ojson.NewDoc(req.Payload), true, "", "")
		var reject antigravityOAuthSafeguardError
		if !errors.As(err, &reject) {
			t.Fatalf("expected safeguard rejection, got %v", err)
		}
	})

	t.Run("raw preparation refused", func(t *testing.T) {
		httpReq, errReq := http.NewRequestWithContext(t.Context(), http.MethodPost, antigravityBaseURLDaily+antigravityGeneratePath, nil)
		if errReq != nil {
			t.Fatal(errReq)
		}
		var reject antigravityOAuthSafeguardError
		if err := exec.PrepareRequest(httpReq, auth); !errors.As(err, &reject) || httpReq.Header.Get("Authorization") != "" {
			t.Fatalf("raw preparation: error=%v", err)
		}
	})

	t.Run("guard checks the final payload", func(t *testing.T) {
		filtered := *cfg
		filtered.Payload.Filter = []config.PayloadFilterRule{{
			Models: []config.PayloadModelRule{{Name: req.Model}},
			Params: []string{"contents"},
		}}
		ctx := helps.WithPayloadFinalizer(t.Context(), helps.NewPayloadFinalizer(&filtered, "antigravity", req.Model, "antigravity", "request", req.Payload, req, opts))
		doc := ojson.NewDoc(req.Payload)
		executor := NewAntigravityExecutor(&filtered)
		executor.referenceLoader = loadReference
		_, _, err := executor.buildRequestDoc(ctx, auth, "test-token", req.Model, doc, true, "", "")
		var reject antigravityOAuthSafeguardError
		if !errors.As(err, &reject) || doc.Get("request.contents").Exists() {
			t.Fatalf("final filter or guard did not run: %v", err)
		}
	})
}
