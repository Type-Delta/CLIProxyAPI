package api

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestViewerCORSRejectsCrossOriginPreflight(t *testing.T) {
	engine := gin.New()
	engine.Use(corsMiddleware())
	engine.OPTIONS("/v0/analytics/viewer/session", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	request := httptest.NewRequest(http.MethodOptions, "http://cpa.test/v0/analytics/viewer/session", nil)
	request.Host = "cpa.test"
	request.Header.Set("Origin", "https://attacker.test")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
	if value := response.Header().Get("Access-Control-Allow-Origin"); value != "" {
		t.Fatalf("cross-origin response exposed CORS origin %q", value)
	}
}

func TestViewerCORSPreservesSameOriginCredentials(t *testing.T) {
	engine := gin.New()
	engine.Use(corsMiddleware())
	engine.GET("/v0/analytics/viewer/summary", func(c *gin.Context) { c.Status(http.StatusOK) })

	request := httptest.NewRequest(http.MethodGet, "https://cpa.test/v0/analytics/viewer/summary", nil)
	request.Host = "cpa.test"
	request.Header.Set("Origin", "https://cpa.test")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if value := response.Header().Get("Access-Control-Allow-Origin"); value != "https://cpa.test" {
		t.Fatalf("same-origin CORS value = %q", value)
	}
	if value := response.Header().Get("Access-Control-Allow-Credentials"); value != "true" {
		t.Fatalf("credentials header = %q", value)
	}
}

func TestViewerCORSPreservesLoopbackHTTPWithPortOverTCP(t *testing.T) {
	security, err := newAnalyticsViewerSecurity(AnalyticsViewerSecurityOptions{AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.Use(corsMiddlewareWithViewerSecurity(security))
	engine.POST("/v0/analytics/viewer/session", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpListener := newMuxListener(listener.Addr(), 16)
	apiServer := &Server{}
	serveErrors := make(chan error, 2)
	go func() { serveErrors <- apiServer.acceptMuxConnections(listener, httpListener) }()
	server := &http.Server{Handler: engine}
	go func() { serveErrors <- server.Serve(httpListener) }()
	t.Cleanup(func() {
		if errClose := server.Close(); errClose != nil && !errors.Is(errClose, http.ErrServerClosed) {
			t.Errorf("close HTTP server: %v", errClose)
		}
		if errClose := httpListener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			t.Errorf("close HTTP listener: %v", errClose)
		}
		if errClose := listener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			t.Errorf("close base listener: %v", errClose)
		}
	})
	serverURL := "http://" + listener.Addr().String()
	request, err := http.NewRequest(http.MethodPost, serverURL+"/v0/analytics/viewer/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", serverURL)
	response, err := http.DefaultClient.Do(request)
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
}

func TestViewerCORSRejectsMalformedOrigins(t *testing.T) {
	for _, origin := range []string{
		"https://user@cpa.test",
		"https://cpa.test/path",
		"https://cpa.test?query=value",
		"https://cpa.test#fragment",
	} {
		t.Run(origin, func(t *testing.T) {
			engine := gin.New()
			engine.Use(corsMiddleware())
			engine.GET("/v0/analytics/viewer/summary", func(c *gin.Context) { c.Status(http.StatusOK) })

			request := httptest.NewRequest(http.MethodGet, "https://cpa.test/v0/analytics/viewer/summary", nil)
			request.Header.Set("Origin", origin)
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
			}
		})
	}
}

func TestViewerCORSRejectsMultipleOriginHeaders(t *testing.T) {
	engine := gin.New()
	engine.Use(corsMiddleware())
	engine.GET("/v0/analytics/viewer/summary", func(c *gin.Context) { c.Status(http.StatusOK) })

	request := httptest.NewRequest(http.MethodGet, "https://cpa.test/v0/analytics/viewer/summary", nil)
	request.Header.Add("Origin", "https://cpa.test")
	request.Header.Add("Origin", "https://attacker.test")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestViewerCORSUsesForwardedHTTPSOnlyFromTrustedPeer(t *testing.T) {
	security, err := newAnalyticsViewerSecurity(AnalyticsViewerSecurityOptions{TrustedProxyCIDRs: []string{"172.30.0.0/24"}})
	if err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.Use(corsMiddlewareWithViewerSecurity(security))
	engine.GET("/v0/analytics/viewer/summary", func(c *gin.Context) { c.Status(http.StatusOK) })

	for _, test := range []struct {
		name       string
		remoteAddr string
		wantStatus int
	}{
		{name: "trusted", remoteAddr: "172.30.0.8:41321", wantStatus: http.StatusOK},
		{name: "untrusted", remoteAddr: "198.51.100.8:41321", wantStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://cpa.test/v0/analytics/viewer/summary", nil)
			request.Host = "cpa.test"
			request.RemoteAddr = test.remoteAddr
			request.Header.Set("Origin", "https://cpa.test")
			request.Header.Set("X-Forwarded-Proto", "https")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}
