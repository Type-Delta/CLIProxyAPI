package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpwire"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

type r3ErrorTransport struct {
	*httptest.ResponseRecorder
	legacy, errorAware int
}

func (w *r3ErrorTransport) Flush()            { w.legacy++; w.ResponseRecorder.Flush() }
func (w *r3ErrorTransport) FlushError() error { w.errorAware++; return io.ErrClosedPipe }
func TestR3OwnedWriterPreservesFlushError(t *testing.T) {
	for _, owned := range []bool{false, true} {
		name := "baseline"
		if owned {
			name = "owned"
		}
		t.Run(name, func(t *testing.T) {
			e := gin.New()
			s := &Server{}
			if owned {
				e.Use(s.ownedHijacksMiddleware())
			}
			var got error
			e.GET("/", func(c *gin.Context) {
				c.Header("X-Test", "kept")
				c.Status(http.StatusAccepted)
				got = httpwire.FlushResponse(c.Writer)
			})
			transport := &r3ErrorTransport{ResponseRecorder: httptest.NewRecorder()}
			e.ServeHTTP(transport, httptest.NewRequest("GET", "/", nil))
			if !errors.Is(got, io.ErrClosedPipe) {
				t.Fatalf("flush error lost: got=%v errorAware=%d legacy=%d", got, transport.errorAware, transport.legacy)
			}
			if transport.Code != http.StatusAccepted || transport.Header().Get("X-Test") != "kept" {
				t.Fatal("header behavior changed")
			}
		})
	}
}
func TestR3OwnedWriterPreservesHTTP2(t *testing.T) {
	e := gin.New()
	s := &Server{}
	e.Use(s.ownedHijacksMiddleware())
	e.GET("/", func(c *gin.Context) {
		c.Header("X-Test", "kept")
		c.Status(202)
		_, _ = c.Writer.Write([]byte("first"))
		c.Writer.Flush()
		_, _ = c.Writer.Write([]byte("second"))
	})
	ts := httptest.NewUnstartedServer(e)
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ProtoMajor != 2 || resp.StatusCode != 202 || resp.Header.Get("X-Test") != "kept" || string(body) != "firstsecond" {
		t.Fatalf("unexpected HTTP2 response %s %d %q", resp.Proto, resp.StatusCode, body)
	}
	s.closeOwnedHijacks()
}

func TestR3ClosedClientFlushThroughRealMiddleware(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.RequestLog = true
	flushed := make(chan error, 1)
	server := NewServer(cfg, nil, nil, "", WithRouterConfigurator(func(engine *gin.Engine, _ *handlers.BaseAPIHandler, _ *config.Config) {
		engine.POST("/v1/flush-probe", middleware.CaptureAuthenticatedRequestBody, func(c *gin.Context) {
			c.Header("Content-Type", "text/event-stream")
			_, _ = c.Writer.Write([]byte("data: ready\n\n"))
			if err := httpwire.FlushResponse(c.Writer); err != nil {
				flushed <- err
				return
			}
			<-c.Request.Context().Done()
			_, _ = c.Writer.Write([]byte("data: after disconnect\n\n"))
			flushed <- httpwire.FlushResponse(c.Writer)
		})
	}))
	ts := httptest.NewServer(server.engine)
	defer ts.Close()
	server.server = ts.Config
	defer server.Stop(context.Background())
	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = io.WriteString(conn, "POST /v1/flush-probe HTTP/1.1\r\nHost: localhost\r\nContent-Length: 2\r\nContent-Type: application/json\r\n\r\n{}")
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err = bufio.NewReader(response.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if err = conn.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	select {
	case err := <-flushed:
		if err == nil {
			t.Fatal("closed client transport error was masked by middleware")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not observe client disconnect")
	}
}
