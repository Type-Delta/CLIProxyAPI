package antigravitycapture

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/MoeForge/moego/regexp"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/referencecapture"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

var cloudCodeHosts = []string{
	"cloudcode-pa.googleapis.com",
	"daily-cloudcode-pa.googleapis.com",
	"daily-cloudcode-pa.sandbox.googleapis.com",
	"autopush-cloudcode-pa.sandbox.googleapis.com",
}

var auxiliaryHosts = []string{"www.googleapis.com", "openidconnect.googleapis.com", "aicode.googleapis.com", "play.googleapis.com"}

var userAgentPattern = regexp.MustCompile(`^antigravity/(?:(?:cli|hub)/[0-9]+\.[0-9]+\.[0-9]+ \(aidev_client; os_type=[a-z]+; arch=[a-z0-9_]+(?:; [a-z_]+=[a-zA-Z0-9_.-]+)*\)|[0-9]+\.[0-9]+\.[0-9]+(?: [a-z]+/[a-z0-9_]+| \([a-zA-Z0-9_ /.-]+\)))$`)

func IsNativeUserAgent(value string) bool {
	return userAgentPattern.MatchString(strings.TrimSpace(value))
}

// IsDirectURL identifies the Google Cloud Code endpoints for Antigravity OAuth.
func IsDirectURL(target *url.URL) bool {
	return target != nil && target.Scheme == "https" && target.User == nil && target.Opaque == "" &&
		(target.Port() == "" || target.Port() == "443") && target.Fragment == "" &&
		slices.Contains(cloudCodeHosts, strings.ToLower(target.Hostname()))
}

func capture(ctx context.Context, binary, version string, credential Credential) (Profile, error) {
	if credential.AccessToken == "" || len(credential.AccessToken) > 8192 || strings.ContainsAny(credential.AccessToken, "\r\n") ||
		!credential.ExpiresAt.After(time.Now()) || credential.Project == "" {
		return Profile{}, errors.New("capture requires a current Antigravity access token and project")
	}
	home, err := os.MkdirTemp("", "cpa-antigravity-probe-")
	if err != nil {
		return Profile{}, err
	}
	defer os.RemoveAll(home)
	if err := securePrivateDirectory(home); err != nil {
		return Profile{}, fmt.Errorf("secure private Antigravity probe directory: %w", err)
	}
	tokenDir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(tokenDir, 0700); err != nil {
		return Profile{}, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Profile{}, err
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cert, caPEM, err := referencecapture.Certificate("CPA temporary Antigravity capture CA", append(slices.Clone(cloudCodeHosts), auxiliaryHosts...))
	if err != nil {
		return Profile{}, err
	}
	caPath := filepath.Join(home, "capture-ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		return Profile{}, err
	}
	transport, _, err := proxyutil.BuildHTTPTransport(credential.ProxyURL)
	if err != nil {
		return Profile{}, errors.New("invalid Antigravity credential proxy configuration")
	}
	if transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
	}
	defer transport.CloseIdleConnections()
	proxy := &captureProxy{
		ctx: ctx, tls: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		version: version, credential: credential, transport: transport, profiles: make(chan Profile, 1),
		failures: make(chan error, 1),
	}
	server := &http.Server{Handler: proxy}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	env := captureEnvironment(home, "http://"+listener.Addr().String(), caPath)
	// Bound the lifetime of the private child, including startup and version
	// reporting. The relay does not add per-request network timeouts.
	childCtx, stopChild := context.WithTimeout(ctx, 2*time.Minute)
	defer stopChild()
	check := exec.CommandContext(childCtx, binary, "--version")
	check.Dir, check.Env = home, env
	if output, err := check.Output(); err != nil || strings.TrimSpace(string(output)) != version {
		return Profile{}, errors.New("private Antigravity binary version differs from its manifest")
	}
	// The official CLI uses its file token store for SSH sessions. Write only
	// the live access token, so the child cannot rotate or persist CPA's refresh token.
	tokenFile := filepath.Join(tokenDir, "antigravity-oauth-token")
	if err := writePrivateJSON(tokenFile, map[string]any{
		"auth_method": "consumer",
		"token": map[string]any{
			"access_token": credential.AccessToken, "token_type": "Bearer", "expiry": credential.ExpiresAt,
		},
	}); err != nil {
		return Profile{}, err
	}
	command := exec.CommandContext(childCtx, binary, "--mode", "plan", "--print", "Reply with exactly OK. Do not use tools.")
	command.Dir, command.Env = home, env
	// CLI diagnostics can contain OAuth state, account data, or request content.
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Run(); err != nil {
		if childCtx.Err() != nil {
			return Profile{}, childCtx.Err()
		}
		select {
		case failure := <-proxy.failures:
			return Profile{}, failure
		default:
		}
		return Profile{}, fmt.Errorf("private Antigravity CLI probe exited unsuccessfully: %w", err)
	}
	select {
	case profile := <-proxy.profiles:
		return profile, nil
	case failure := <-proxy.failures:
		return Profile{}, failure
	default:
		return Profile{}, errors.New("private Antigravity CLI completed without a validated OAuth inference request")
	}
}

func captureEnvironment(home, proxy, caPath string) []string {
	env := []string{
		"PATH=" + home,
		"HOME=" + home, "USERPROFILE=" + home,
		"APPDATA=" + home, "LOCALAPPDATA=" + home,
		"XDG_CONFIG_HOME=" + home, "XDG_DATA_HOME=" + home, "XDG_CACHE_HOME=" + home, "XDG_STATE_HOME=" + home,
		"TMPDIR=" + home, "TMP=" + home, "TEMP=" + home,
		"SSH_CONNECTION=127.0.0.1 0 127.0.0.1 0",
		"ANTIGRAVITY_CREDENTIAL_SCOPE=" + filepath.Base(home),
		"AGY_CLI_DISABLE_AUTO_UPDATE=true",
		"HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy,
		"AGY_CA_CERT=" + caPath, "SSL_CERT_FILE=" + caPath,
	}
	for _, key := range []string{"SYSTEMROOT", "WINDIR", "LANG", "LC_ALL", "TZ"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

type captureProxy struct {
	ctx        context.Context
	tls        *tls.Config
	version    string
	credential Credential
	transport  http.RoundTripper
	profiles   chan Profile
	failures   chan error
}

func (p *captureProxy) ServeHTTP(w http.ResponseWriter, connect *http.Request) {
	host, port, err := net.SplitHostPort(connect.Host)
	host = strings.ToLower(host)
	if connect.Method != http.MethodConnect || err != nil || port != "443" ||
		(!slices.Contains(cloudCodeHosts, host) && !slices.Contains(auxiliaryHosts, host)) {
		http.Error(w, "capture destination is unavailable", http.StatusForbidden)
		return
	}
	conn, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	stop := context.AfterFunc(p.ctx, func() { _ = conn.Close() })
	defer stop()
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil || buffered.Flush() != nil {
		return
	}
	if buffered.Reader.Buffered() != 0 {
		return
	}
	secured := tls.Server(conn, p.tls)
	if err := secured.HandshakeContext(p.ctx); err != nil || !strings.EqualFold(secured.ConnectionState().ServerName, host) {
		log.WithField("host", host).Debug("Antigravity reference client TLS handshake did not complete")
		return
	}
	req, err := http.ReadRequest(bufio.NewReader(secured))
	if err == nil {
		p.forward(secured, req, host)
	}
}

func (p *captureProxy) forward(conn io.Writer, req *http.Request, host string) bool {
	defer req.Body.Close()
	if !strings.EqualFold(req.Host, host) && !strings.EqualFold(req.Host, host+":443") {
		return false
	}
	path := req.URL.EscapedPath()
	inference := slices.Contains(cloudCodeHosts, host) && (path == "/v1internal:generateContent" || path == "/v1internal:streamGenerateContent")
	auth := req.Header.Values("Authorization")
	if (len(auth) > 0 || inference) && (len(auth) != 1 || subtle.ConstantTimeCompare([]byte(auth[0]), []byte("Bearer "+p.credential.AccessToken)) != 1) {
		log.WithField("host", host).Debug("Antigravity reference client did not use the selected credential")
		return false
	}
	if (host == "www.googleapis.com" || host == "openidconnect.googleapis.com") &&
		(req.Method != http.MethodGet || (path != "/oauth2/v2/userinfo" && path != "/v1/userinfo")) {
		return false
	}
	outgoing := req.Clone(p.ctx)
	outgoing.URL.Scheme, outgoing.URL.Host, outgoing.RequestURI = "https", host, ""
	outgoing.Header.Del("Accept-Encoding")
	outgoing.Header.Del("Connection")
	outgoing.Header.Del("Proxy-Authorization")
	outgoing.Header.Del("Proxy-Connection")
	const maxProbeBody = 4 << 20
	body, err := io.ReadAll(io.LimitReader(req.Body, maxProbeBody+1))
	if err != nil || len(body) > maxProbeBody {
		return false
	}
	outgoing.Body, outgoing.ContentLength = http.NoBody, int64(len(body))
	if len(body) != 0 {
		outgoing.Body = io.NopCloser(bytes.NewReader(body))
	}
	outgoing.TransferEncoding = nil
	outgoing.Header.Del("Transfer-Encoding")
	var profile Profile
	if inference {
		if req.Method != http.MethodPost {
			return false
		}
		if !ojson.ValidBytes(body) {
			return false
		}
		doc := ojson.NewDoc(body)
		contentType, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
		profile = Profile{Version: p.version, CapturedAt: time.Now().UTC(),
			UserAgent: req.Header.Get("User-Agent"), ContentType: contentType, EnvelopeUserAgent: doc.Get("userAgent").Str}
		if !profile.valid() || doc.Get("project").Str != p.credential.Project || doc.Get("requestType").Str != "agent" ||
			doc.Get("model").Str == "" || doc.Get("requestId").Str == "" || doc.Get("request.sessionId").Str == "" ||
			!doc.Get("request.contents").IsArray() {
			log.WithField("host", host).Debug("Antigravity reference inference did not match the expected profile")
			return false
		}
	}
	resp, err := p.transport.RoundTrip(outgoing)
	if err != nil {
		log.WithField("host", host).Debug("Antigravity reference upstream request failed")
		return false
	}
	log.Debugf("Antigravity reference upstream response: host=%s status=%d inference=%t", host, resp.StatusCode, inference)
	defer resp.Body.Close()
	if inference {
		const maxProbeResponse = 1 << 20
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeResponse+1))
		if err != nil || len(body) > maxProbeResponse {
			return false
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.TransferEncoding = nil
		resp.Header.Del("Transfer-Encoding")
		resp.Header.Del("Content-Length")
		if resp.StatusCode != http.StatusOK {
			select {
			case p.failures <- probeResponseError(resp.StatusCode, body):
			default:
			}
		} else if completedProbe(body) {
			select {
			case p.profiles <- profile:
			default:
			}
		}
	}
	resp.Proto, resp.ProtoMajor, resp.ProtoMinor, resp.Close = "HTTP/1.1", 1, 1, true
	return resp.Write(conn) == nil
}

func probeResponseError(status int, body []byte) error {
	if status == http.StatusForbidden {
		for _, detail := range ojson.GetBytesNoCopy(body, "error.details").Array() {
			if detail.Get("@type").Str == "type.googleapis.com/google.rpc.ErrorInfo" && detail.Get("reason").Str == "TOS_VIOLATION" {
				return errors.New("Google refused the private Antigravity reference probe with HTTP 403 (TOS_VIOLATION)")
			}
		}
	}
	return fmt.Errorf("Google refused the private Antigravity reference probe with HTTP %d", status)
}

func completedProbe(body []byte) bool {
	var text, stop bool
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		candidate := ojson.GetBytesNoCopy(line, "response.candidates.0")
		stop = stop || candidate.Get("finishReason").Str == "STOP"
		for _, part := range candidate.Get("content.parts").Array() {
			if !part.Get("thought").Bool() && strings.TrimSpace(part.Get("text").Str) != "" {
				text = true
			}
		}
	}
	return text && stop
}
