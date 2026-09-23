package claudecapture

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Capture runs the installed Claude Code with a fixed harmless prompt through a
// loopback HTTPS proxy, relays its request to Anthropic, and writes a sanitized
// reference. A genuine OAuth login must already exist in Claude Code.
func Capture(ctx context.Context, cli, outputPath string) error {
	if cli == "" {
		cli = "claude"
	}
	version, err := versionOf(cli)
	if err != nil {
		return err
	}
	if err := requireOAuthLogin(cli); err != nil {
		return err
	}
	privateDir, err := os.MkdirTemp("", "cpa-claude-capture-")
	if err != nil {
		return fmt.Errorf("create private capture directory: %w", err)
	}
	defer os.RemoveAll(privateDir)
	if err := os.Chmod(privateDir, 0700); err != nil {
		return err
	}
	cert, caPEM, err := interceptionCertificate()
	if err != nil {
		return err
	}
	caPath := privateDir + "/ca.pem"
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		return fmt.Errorf("write temporary CA: %w", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start local capture proxy: %w", err)
	}
	defer listener.Close()
	profiles := make(chan *Profile, 64)
	server := &http.Server{Handler: &captureProxy{cert: cert, version: version, profiles: profiles}}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	command := exec.CommandContext(ctx, cli, "--print", "Reply with OK.", "--no-session-persistence", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--model", "haiku", "--tools", "")
	command.Dir = privateDir
	command.Env = proxyEnvironment(os.Environ(), listener.Addr().String(), caPath)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return fmt.Errorf("Claude Code probe failed; check OAuth login and local proxy trust: %w", err)
	}
	var merged *Profile
	for {
		select {
		case profile := <-profiles:
			if merged == nil {
				merged = profile
			} else {
				merged.Variants = append(merged.Variants, profile.Variants...)
			}
		default:
			if merged == nil {
				return errors.New("Claude Code completed without an intercepted OAuth Messages request")
			}
			merged.Variants = uniqueVariants(merged.Variants)
			return writeProfile(outputPath, merged)
		}
	}
}

func uniqueVariants(input []Variant) []Variant {
	seen := make(map[string]bool)
	output := make([]Variant, 0, len(input))
	for _, variant := range input {
		key := variant.Endpoint + fmt.Sprint(variant.Stream) + variant.Beta + strings.Join(variant.BodyKeys, ",")
		if !seen[key] {
			seen[key] = true
			output = append(output, variant)
		}
	}
	return output
}

func requireOAuthLogin(cli string) error {
	out, err := exec.Command(cli, "auth", "status").Output()
	if err != nil {
		return errors.New("Claude Code is not logged in; run claude auth login first")
	}
	var status struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
		Provider   string `json:"apiProvider"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return errors.New("cannot parse Claude Code auth status")
	}
	if !status.LoggedIn || (status.AuthMethod != "claude.ai" && status.AuthMethod != "oauth") || status.Provider != "firstParty" {
		return errors.New("Claude Code must have a first-party claude.ai OAuth login")
	}
	return nil
}

func proxyEnvironment(base []string, address, caPath string) []string {
	var env []string
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok && (strings.EqualFold(key, "HTTPS_PROXY") || strings.EqualFold(key, "HTTP_PROXY") || strings.EqualFold(key, "ALL_PROXY") || strings.EqualFold(key, "NO_PROXY") || strings.EqualFold(key, "NODE_EXTRA_CA_CERTS") || strings.EqualFold(key, "ANTHROPIC_BASE_URL") || strings.EqualFold(key, "ANTHROPIC_API_KEY") || strings.EqualFold(key, "ANTHROPIC_AUTH_TOKEN")) {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "HTTPS_PROXY=http://"+address, "HTTP_PROXY=http://"+address, "ALL_PROXY=http://"+address, "NO_PROXY=", "NODE_EXTRA_CA_CERTS="+caPath, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_TELEMETRY=1")
}

type captureProxy struct {
	cert     tls.Certificate
	version  string
	profiles chan<- *Profile
}

func captureTrace(event string) {
	if os.Getenv("CPA_CAPTURE_DEBUG") == "1" {
		fmt.Fprintln(os.Stderr, "claude capture:", event)
	}
}

func (p *captureProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodConnect {
		http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT unavailable", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	if !strings.EqualFold(req.Host, "api.anthropic.com:443") {
		captureTrace("tunneling non-API host " + req.Host)
		p.tunnel(client, buffered, req.Host)
		return
	}
	captureTrace("intercepting API CONNECT")
	_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	_ = buffered.Flush()
	tlsClient := tls.Server(client, &tls.Config{Certificates: []tls.Certificate{p.cert}, MinVersion: tls.VersionTLS12})
	if err := tlsClient.Handshake(); err != nil {
		captureTrace("client TLS handshake failed")
		return
	}
	captureTrace("client TLS handshake completed")
	defer tlsClient.Close()
	reader := bufio.NewReader(tlsClient)
	for {
		incoming, err := http.ReadRequest(reader)
		if err != nil {
			captureTrace("API request read ended")
			return
		}
		if incoming.URL.Path == "/v1/messages" {
			captureTrace("received Messages request")
		} else if incoming.URL.Path == "/v1/messages/count_tokens" {
			captureTrace("received count_tokens request")
		} else {
			captureTrace("received other API request")
		}
		body, err := io.ReadAll(io.LimitReader(incoming.Body, 32<<20+1))
		_ = incoming.Body.Close()
		if err != nil || len(body) > 32<<20 {
			return
		}
		if profile := profileFromRequest(incoming, body, p.version); profile != nil {
			captureTrace("captured OAuth Messages request")
			select {
			case p.profiles <- profile:
			default:
			}
		}
		outgoing, err := http.NewRequestWithContext(incoming.Context(), incoming.Method, "https://api.anthropic.com"+incoming.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			return
		}
		outgoing.Header = incoming.Header.Clone()
		outgoing.Host = "api.anthropic.com"
		outgoing.ContentLength = int64(len(body))
		response, err := http.DefaultTransport.RoundTrip(outgoing)
		if err != nil {
			captureTrace("upstream relay failed")
			_, _ = io.WriteString(tlsClient, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
			return
		}
		captureTrace(fmt.Sprintf("upstream response %d", response.StatusCode))
		if os.Getenv("CPA_CAPTURE_DEBUG") == "1" && incoming.URL.Path == "/v1/messages" {
			captureTrace(fmt.Sprintf("Messages response type=%s encoding=%s length=%d", response.Header.Get("Content-Type"), response.Header.Get("Content-Encoding"), response.ContentLength))
			response.Body = &responseTracker{ReadCloser: response.Body}
		}
		response.Proto = "HTTP/1.1"
		response.ProtoMajor = 1
		response.ProtoMinor = 1
		response.Close = true
		if err := response.Write(tlsClient); err != nil {
			captureTrace(fmt.Sprintf("writing upstream response failed: %T: %v", err, err))
			_ = response.Body.Close()
			return
		}
		captureTrace("upstream response sent to Claude Code")
		if tracked, ok := response.Body.(*responseTracker); ok {
			captureTrace(fmt.Sprintf("Messages response bytes=%d stop-event=%t", tracked.count, tracked.hasStop(response.Header.Get("Content-Encoding"))))
		}
		_ = response.Body.Close()
		if incoming.Close || response.Close {
			return
		}
	}
}

type responseTracker struct {
	io.ReadCloser
	count   int64
	content bytes.Buffer
}

func (t *responseTracker) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	t.count += int64(n)
	if n > 0 && t.content.Len() < 128<<10 {
		_, _ = t.content.Write(p[:n])
	}
	return n, err
}

func (t *responseTracker) hasStop(encoding string) bool {
	if encoding != "gzip" {
		return bytes.Contains(t.content.Bytes(), []byte("event: message_stop"))
	}
	reader, err := gzip.NewReader(bytes.NewReader(t.content.Bytes()))
	if err != nil {
		return false
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, 1<<20))
	return err == nil && bytes.Contains(decoded, []byte("event: message_stop"))
}

func (p *captureProxy) tunnel(client net.Conn, buffered *bufio.ReadWriter, target string) {
	upstream, err := net.Dial("tcp", target)
	if err != nil {
		captureTrace("non-API tunnel upstream dial failed")
		_, _ = buffered.WriteString("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		_ = buffered.Flush()
		return
	}
	captureTrace("non-API tunnel upstream connected")
	defer upstream.Close()
	_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	_ = buffered.Flush()
	go func() {
		copyTunnel(upstream, buffered, "non-API tunnel client data")
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	copyTunnel(client, upstream, "non-API tunnel upstream data")
}

func copyTunnel(dst io.Writer, src io.Reader, event string) {
	buffer := make([]byte, 32<<10)
	first := true
	for {
		n, err := src.Read(buffer)
		if n > 0 {
			if first {
				captureTrace(event)
				first = false
			}
			if _, writeErr := dst.Write(buffer[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func profileFromRequest(req *http.Request, body []byte, version string) *Profile {
	endpoint := ""
	switch req.URL.Path {
	case "/v1/messages":
		endpoint = "messages"
	case "/v1/messages/count_tokens":
		endpoint = "count_tokens"
	}
	if req.Method != http.MethodPost || endpoint == "" || req.Header.Get("Authorization") == "" {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || len(fields) == 0 || len(fields["messages"]) == 0 || len(fields["model"]) == 0 {
		return nil
	}
	profile := &Profile{SchemaVersion: schemaVersion, ClaudeVersion: version, CapturedAt: time.Now().UTC(), Headers: make(map[string]string), Variants: []Variant{{Endpoint: endpoint, Beta: req.Header.Get("Anthropic-Beta")}}}
	for key := range fields {
		profile.Variants[0].BodyKeys = append(profile.Variants[0].BodyKeys, key)
	}
	sort.Strings(profile.Variants[0].BodyKeys)
	if raw, ok := fields["stream"]; ok {
		if json.Unmarshal(raw, &profile.Variants[0].Stream) != nil {
			return nil
		}
	}
	for key := range allowedHeaders {
		if value := req.Header.Get(key); value != "" {
			profile.Headers[key] = value
		}
	}
	if !hasBeta(profile.Headers["Anthropic-Beta"], "oauth-2025-04-20") || profile.validate(version) != nil {
		return nil
	}
	return profile
}

func interceptionCertificate() (tls.Certificate, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	caTemplate := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "CPA temporary Claude capture CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serial, err = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leafTemplate := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "api.anthropic.com"}, DNSNames: []string{"api.anthropic.com"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), nil
}
