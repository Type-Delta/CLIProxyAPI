package api

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestAcceptMuxNotBlockedByIdleConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer listener.Close()

	var routed atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routed.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewUnstartedServer(handler)
	defer srv.Close()

	muxLn := newMuxListener(listener.Addr(), 1024)
	server := &Server{managementRoutesEnabled: atomic.Bool{}}
	server.managementRoutesEnabled.Store(false)

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.acceptMuxConnections(listener, muxLn)
	}()

	srv.Listener = muxLn
	srv.Start()

	// Open an idle TCP connection that never sends any bytes.
	idleConn, err := net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to dial idle connection: %v", err)
	}
	defer idleConn.Close()

	// Give the accept loop time to pick up the idle connection.
	time.Sleep(50 * time.Millisecond)

	// Send a real HTTP request. Before the fix, the accept loop would be
	// blocked on Peek(1) for the idle connection, causing this request to
	// time out.
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + listener.Addr().String() + "/")
	if err != nil {
		listener.Close()
		t.Fatalf("HTTP request failed (accept loop may be blocked by idle connection): %v", err)
	}
	resp.Body.Close()

	listener.Close()

	if routed.Load() == 0 {
		t.Error("expected at least one request to be routed")
	}
}

func TestProductionMuxPreservesTLSWithoutALPN(t *testing.T) {
	gin.SetMode(gin.TestMode)
	security, err := newAnalyticsViewerSecurity(AnalyticsViewerSecurityOptions{})
	if err != nil {
		t.Fatal(err)
	}

	engine := gin.New()
	engine.Use(corsMiddlewareWithViewerSecurity(security))
	requestTLS := make(chan *tls.ConnectionState, 1)
	engine.POST("/v0/analytics/viewer/session", func(c *gin.Context) {
		requestTLS <- c.Request.TLS
		if !security.requestAllowsSession(c.Request) {
			c.Status(http.StatusUnauthorized)
			return
		}
		c.Status(http.StatusNoContent)
	})

	certificateServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificate := certificateServer.TLS.Certificates[0]
	certificateServer.Close()

	baseListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		NextProtos:   []string{"h2", "http/1.1"},
	}
	tlsListener := tls.NewListener(baseListener, tlsConfig)
	httpListener := newMuxListener(tlsListener.Addr(), 16)
	apiServer := &Server{}
	server := &http.Server{Handler: engine, TLSConfig: tlsConfig}
	serveErrors := make(chan error, 2)
	go func() { serveErrors <- apiServer.acceptMuxConnections(tlsListener, httpListener) }()
	go func() { serveErrors <- server.Serve(httpListener) }()
	t.Cleanup(func() {
		if errClose := server.Close(); errClose != nil && !errors.Is(errClose, http.ErrServerClosed) {
			t.Errorf("close HTTP server: %v", errClose)
		}
		if errClose := httpListener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			t.Errorf("close HTTP listener: %v", errClose)
		}
		if errClose := tlsListener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			t.Errorf("close TLS listener: %v", errClose)
		}
	})

	conn, err := tls.Dial("tcp", tlsListener.Addr().String(), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec -- ephemeral test certificate
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			t.Errorf("close TLS client: %v", errClose)
		}
	}()
	if protocol := conn.ConnectionState().NegotiatedProtocol; protocol != "" {
		t.Fatalf("negotiated ALPN = %q, want empty", protocol)
	}

	serverURL := "https://" + tlsListener.Addr().String()
	request, err := http.NewRequest(http.MethodPost, serverURL+"/v0/analytics/viewer/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", serverURL)
	request.Close = true
	if errWrite := request.Write(conn); errWrite != nil {
		t.Fatal(errWrite)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			t.Errorf("close response body: %v", errClose)
		}
	}()

	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	if value := response.Header.Get("Access-Control-Allow-Origin"); value != serverURL {
		t.Fatalf("same-origin CORS value = %q, want %q", value, serverURL)
	}
	if value := response.Header.Get("Access-Control-Allow-Credentials"); value != "true" {
		t.Fatalf("credentials header = %q, want true", value)
	}
	select {
	case state := <-requestTLS:
		if state == nil {
			t.Fatal("Request.TLS is nil")
		}
		if state.NegotiatedProtocol != "" {
			t.Fatalf("request negotiated ALPN = %q, want empty", state.NegotiatedProtocol)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach viewer handler")
	}
}
