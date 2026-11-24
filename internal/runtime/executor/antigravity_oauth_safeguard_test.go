package executor

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/antigravitycapture"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

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
		httpReq, body, err := exec.buildGenerationRequestDoc(t.Context(), auth, "test-token", req.Model, ojson.NewDoc(req.Payload), true, "", "", nil)
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

	t.Run("outbound authentication override refused", func(t *testing.T) {
		custom := auth.Clone()
		custom.Attributes = map[string]string{"header:Authorization": "Bearer different-token"}
		_, _, err := exec.buildGenerationRequestDoc(t.Context(), custom, "test-token", req.Model, ojson.NewDoc(req.Payload), true, "", "", nil)
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
		_, _, err := executor.buildGenerationRequestDoc(ctx, auth, "test-token", req.Model, doc, true, "", "", nil)
		var reject antigravityOAuthSafeguardError
		if !errors.As(err, &reject) || doc.Get("request.contents").Exists() {
			t.Fatalf("final filter or guard did not run: %v", err)
		}
	})
}
