package antigravitycapture

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestCaptureRequiresSelectedTokenAndSuccessfulInference(t *testing.T) {
	const version = "1.5.16"
	const token = "test-access-token"
	const host = "daily-cloudcode-pa.googleapis.com"
	const path = "/v1internal:streamGenerateContent"
	const request = `{"model":"test","project":"fake-project","userAgent":"antigravity","requestId":"fake-request","requestType":"agent","request":{"sessionId":"fake-session","contents":[{"role":"user","parts":[{"text":"test prompt"}]}]}}`
	const response = "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"OK\"}]}}]}}\n\ndata: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\"}]}}\n\n"
	var calls int
	proxy := captureProxy{
		ctx: t.Context(), version: version,
		credential: Credential{AccessToken: token, Project: "fake-project"},
		profiles:   make(chan Profile, 1),
		transport: testTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.URL.Host != host || req.Header.Get("Authorization") != "Bearer "+token {
				t.Fatal("capture forwarded an unexpected destination or credential")
			}
			return &http.Response{
				StatusCode: http.StatusOK, ProtoMajor: 1, ProtoMinor: 1,
				Body: io.NopCloser(strings.NewReader(response)), Header: http.Header{"Content-Type": {"text/event-stream"}},
			}, nil
		}),
	}
	run := func(bearer, payload string) *bytes.Buffer {
		req := httptest.NewRequest(http.MethodPost, "https://"+host+path, strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "antigravity/cli/"+version+" (aidev_client; os_type=linux; arch=amd64)")
		buf := new(bytes.Buffer)
		proxy.forward(buf, req, host)
		return buf
	}
	run("foreign-token", request)
	run(token, strings.Replace(request, "fake-project", "foreign-project", 1))
	if calls != 0 || len(proxy.profiles) != 0 {
		t.Fatal("capture forwarded an unapproved credential or project")
	}
	if result := run(token, request).String(); !strings.Contains(result, "HTTP/1.1 200") || calls != 1 || len(proxy.profiles) != 1 {
		t.Fatal("valid native inference did not produce a capture")
	}
	profile := <-proxy.profiles
	if profile.Version != version || profile.EnvelopeUserAgent != "antigravity" || profile.ContentType != "application/json" {
		t.Fatalf("unexpected captured profile: %+v", profile)
	}
	if completedProbe([]byte("data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\"}]}}")) {
		t.Fatal("terminal metadata alone is not a successful inference")
	}
}

func TestCaptureEnvironmentUsesPrivateHome(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "host-google-credentials")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "host-session-bus")
	t.Setenv("AGY_ACCOUNT", "host-account")
	env := strings.Join(captureEnvironment(t.TempDir(), "http://127.0.0.1:100", "ca.pem"), "\n")
	for _, forbidden := range []string{"host-google-credentials", "host-session-bus", "host-account"} {
		if strings.Contains(env, forbidden) {
			t.Fatal("private CLI inherited host authorization settings")
		}
	}
	for _, required := range []string{"SSH_CONNECTION=", "ANTIGRAVITY_CREDENTIAL_SCOPE=", "HTTPS_PROXY=http://127.0.0.1:100", "AGY_CA_CERT=ca.pem"} {
		if !strings.Contains(env, required) {
			t.Fatalf("private CLI environment is missing %q", required)
		}
	}
}
