package claudecapture

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
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
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

type CaptureOptions struct {
	Executable string
	OAuthToken string
	OutputPath string
	StateDir   string
}

// CapturePrivate probes a CPA-private Claude executable using an OAuth token
// supplied on an inherited pipe. All CLI state stays under StateDir.
func CapturePrivate(ctx context.Context, options CaptureOptions) error {
	if runtime.GOOS == "windows" {
		return errors.New("private Claude OAuth descriptor capture is unavailable on Windows")
	}
	if err := sandboxAvailable(); err != nil {
		return err
	}
	if options.OAuthToken == "" || len(options.OAuthToken) > 8192 || strings.ContainsAny(options.OAuthToken, "\r\n") {
		return errors.New("Claude OAuth token is missing or invalid")
	}
	root, err := filepath.Abs(options.StateDir)
	if err != nil || options.StateDir == "" {
		return errors.New("private Claude capture state directory is required")
	}
	for _, path := range []string{options.Executable, options.OutputPath} {
		abs, err := filepath.Abs(path)
		if err != nil || path == "" || !withinRoot(root, abs) {
			return errors.New("Claude capture paths must stay under the private state directory")
		}
	}
	if _, err := sandboxPath(root, filepath.Dir(options.OutputPath)); err != nil {
		return fmt.Errorf("private capture output directory is invalid: %w", err)
	}
	version, err := versionOfPrivate(options.Executable, root)
	if err != nil {
		return err
	}
	privateDir, err := os.MkdirTemp(root, ".capture-")
	if err != nil {
		return fmt.Errorf("create private capture directory: %w", err)
	}
	defer os.RemoveAll(privateDir)
	if err := os.Chmod(privateDir, 0700); err != nil {
		return err
	}
	if err := preparePrivateHome(privateDir); err != nil {
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
	server := &http.Server{Handler: &captureProxy{cert: cert, version: version, token: options.OAuthToken, profiles: profiles}}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	insideHome, err := sandboxPath(root, privateDir)
	if err != nil {
		return err
	}
	insideCA, err := sandboxPath(root, caPath)
	if err != nil {
		return err
	}
	command, err := sandboxCommand(ctx, root, privateDir, options.Executable, []string{privateDir}, "--print", "Reply with OK.", "--no-session-persistence", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--model", "haiku", "--tools", "")
	if err != nil {
		return err
	}
	command.Env = privateCommandEnvironment(insideHome, listener.Addr().String(), insideCA)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := runWithOAuthDescriptor(command, options.OAuthToken); err != nil {
		return fmt.Errorf("private Claude Code OAuth probe failed: %w", err)
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
			return writeProfile(options.OutputPath, merged)
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

type captureProxy struct {
	cert      tls.Certificate
	version   string
	token     string
	profiles  chan<- *Profile
	transport http.RoundTripper
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
		if subtle.ConstantTimeCompare([]byte(incoming.Header.Get("Authorization")), []byte("Bearer "+p.token)) != 1 {
			captureTrace("rejecting API request with unexpected credential")
			_, _ = io.WriteString(tlsClient, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}
		profile := profileFromRequest(incoming, body, p.version)
		outgoing, err := http.NewRequestWithContext(incoming.Context(), incoming.Method, "https://api.anthropic.com"+incoming.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			return
		}
		outgoing.Header = incoming.Header.Clone()
		outgoing.Host = "api.anthropic.com"
		outgoing.ContentLength = int64(len(body))
		transport := p.transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		response, err := transport.RoundTrip(outgoing)
		if err != nil {
			captureTrace("upstream relay failed")
			_, _ = io.WriteString(tlsClient, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
			return
		}
		captureTrace(fmt.Sprintf("upstream response %d", response.StatusCode))
		if incoming.URL.Path == "/v1/messages" {
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
			completed := tracked.hasCompletedMessage(response.Header.Get("Content-Type"), response.Header.Get("Content-Encoding"))
			captureTrace(fmt.Sprintf("Messages response bytes=%d completed=%t", tracked.count, completed))
			if response.StatusCode == http.StatusOK && completed && profile != nil {
				select {
				case p.profiles <- profile:
				default:
				}
			}
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

func (t *responseTracker) hasCompletedMessage(contentType, encoding string) bool {
	var reader io.Reader = bytes.NewReader(t.content.Bytes())
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
	case "gzip":
		decoded, err := gzip.NewReader(reader)
		if err != nil {
			return false
		}
		defer decoded.Close()
		reader = decoded
	case "deflate":
		decoded, err := zlib.NewReader(reader)
		if err != nil {
			return false
		}
		defer decoded.Close()
		reader = decoded
	case "br":
		reader = brotli.NewReader(reader)
	case "zstd":
		decoded, err := zstd.NewReader(reader)
		if err != nil {
			return false
		}
		defer decoded.Close()
		reader = decoded
	default:
		return false
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, 1<<20))
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(contentType, "text/event-stream"):
		return bytes.Contains(decoded, []byte("event: message_stop"))
	case strings.HasPrefix(contentType, "application/json"):
		var message struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		return json.Unmarshal(decoded, &message) == nil && message.Type == "message" && message.Role == "assistant"
	default:
		return false
	}
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
