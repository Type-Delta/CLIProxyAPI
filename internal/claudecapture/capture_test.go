package claudecapture

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestCaptureProxyRejectsUnexpectedCredentialBeforeRelay(t *testing.T) {
	cert, caPEM, err := interceptionCertificate()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("capture CA is invalid")
	}
	var relayed atomic.Int32
	proxy := httptest.NewServer(&captureProxy{
		cert: cert, token: "sk-ant-oat-selected", version: "2.1.280",
		transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			relayed.Add(1)
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
		}),
	})
	defer proxy.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprint(conn, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, " 200 ") {
		t.Fatalf("CONNECT status = %q, %v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: "api.anthropic.com", RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewBufferString(`{"model":"claude-haiku-4-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer sk-ant-oat-other")
	if err := request.Write(tlsConn); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(tlsConn), request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden || relayed.Load() != 0 {
		t.Fatalf("mismatched credential response = %d, relayed = %d", response.StatusCode, relayed.Load())
	}
}

func TestCaptureRecognizesCompletedCompressedMessages(t *testing.T) {
	sse := []byte("event: message_start\ndata: {}\n\nevent: message_stop\ndata: {}\n\n")
	encoders := map[string]func(*bytes.Buffer) io.WriteCloser{
		"gzip":    func(out *bytes.Buffer) io.WriteCloser { return gzip.NewWriter(out) },
		"deflate": func(out *bytes.Buffer) io.WriteCloser { return zlib.NewWriter(out) },
		"br":      func(out *bytes.Buffer) io.WriteCloser { return brotli.NewWriter(out) },
		"zstd": func(out *bytes.Buffer) io.WriteCloser {
			writer, _ := zstd.NewWriter(out)
			return writer
		},
	}
	for encoding, newWriter := range encoders {
		t.Run(encoding, func(t *testing.T) {
			var encoded bytes.Buffer
			writer := newWriter(&encoded)
			if _, err := writer.Write(sse); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			tracker := &responseTracker{content: encoded}
			if !tracker.hasCompletedMessage("text/event-stream", encoding) {
				t.Fatal("completed SSE response was rejected")
			}
		})
	}
	tracker := &responseTracker{content: *bytes.NewBufferString(`{"type":"message","role":"assistant","content":[]}`)}
	if !tracker.hasCompletedMessage("application/json", "") {
		t.Fatal("successful JSON response was rejected")
	}
	tracker.content = *bytes.NewBufferString(`{"type":"error","error":{"type":"overloaded_error"}}`)
	if tracker.hasCompletedMessage("application/json", "") {
		t.Fatal("error JSON response was accepted")
	}
}
