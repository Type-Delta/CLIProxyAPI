package test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const outageOverloadBody = `{"error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"Overloaded"}}`

func TestCodexOutageRetriesOnOneResponsesConnection(t *testing.T) {
	var attempts atomic.Int32
	proxy, manager := codexOutageProxy(t, func(w http.ResponseWriter, _ *http.Request) {
		switch attempts.Add(1) {
		case 1, 2:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprint(w, outageOverloadBody)
		case 3:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"discarded\"}}\n\n")
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"success\",\"status\":\"completed\",\"output\":[]}}\n\n")
		}
	})
	response, cancel := codexOutageRequest(t, proxy)
	defer cancel()
	defer func() { _ = response.Body.Close() }()
	body, errRead := io.ReadAll(response.Body)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if response.StatusCode != http.StatusOK || attempts.Load() != 4 || !strings.Contains(string(body), `"id":"success"`) || strings.Contains(string(body), "discarded") {
		t.Fatalf("status=%d, attempts=%d, body=%s", response.StatusCode, attempts.Load(), body)
	}
	auth, _ := manager.GetByID("codex-outage")
	if state := auth.ModelStates["gpt-5.4"]; auth.Quota.Exceeded || state != nil && state.Quota.Exceeded {
		t.Fatalf("overload recorded quota exhaustion: %+v", state)
	}
}

func TestCodexOutageKeepsConnectionAliveDuringLongHint(t *testing.T) {
	restore := handlers.SetContinuityKeepAliveIntervalForTest(10 * time.Millisecond)
	defer restore()
	var attempts atomic.Int32
	proxy, manager := codexOutageProxy(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "326")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, outageOverloadBody)
	})
	response, cancel := codexOutageRequest(t, proxy)
	defer cancel()
	defer func() { _ = response.Body.Close() }()
	line, errRead := bufio.NewReader(response.Body).ReadString('\n')
	if errRead != nil || response.StatusCode != http.StatusOK || !strings.HasPrefix(line, ": keep-alive") {
		t.Fatalf("status=%d, first line=%q, read error=%v", response.StatusCode, line, errRead)
	}
	auth, _ := manager.GetByID("codex-outage")
	state := auth.ModelStates["gpt-5.4"]
	if auth.Quota.Exceeded || state == nil || state.Quota.Exceeded || time.Until(state.NextRetryAfter) < 5*time.Minute {
		t.Fatalf("expected a model overload cooldown without quota exhaustion, got %+v", state)
	}
	cancel()
	if attempts.Load() != 1 {
		t.Fatalf("retried before Retry-After elapsed, attempts=%d", attempts.Load())
	}
}

func TestCodexOutageRetriesOnOneWebsocket(t *testing.T) {
	for _, steering := range []bool{false, true} {
		t.Run(fmt.Sprintf("steering=%t", steering), func(t *testing.T) {
			var attempts atomic.Int32
			upgrader := websocket.Upgrader{}
			cfg := &config.Config{}
			cfg.Codex.ResponseSteering = steering
			cfg.CodexResponseSteering = steering
			proxy, _ := codexOutageProxyWithConfig(t, cfg, true, func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = conn.Close() }()
				attempt := attempts.Add(1)
				if attempt == 1 {
					// Reset after upgrade without reading the client's response.create.
					if errLinger := conn.UnderlyingConn().(*net.TCPConn).SetLinger(0); errLinger != nil {
						t.Error(errLinger)
					}
					return
				}
				if _, _, errRead := conn.ReadMessage(); errRead != nil {
					t.Error(errRead)
					return
				}
				switch attempt {
				case 2:
					// Lose the socket before the client has received a response event.
					return
				case 3:
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":429,"headers":{"Retry-After":"1"},"error":{"message":"Selected model is at capacity. Please try a different model."}}`))
					_, _, _ = conn.ReadMessage()
				default:
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"success"}}`))
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"success","status":"completed","output":[]}}`))
					_, _, _ = conn.ReadMessage()
				}
			})
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if errDeadline := conn.SetReadDeadline(time.Now().Add(8 * time.Second)); errDeadline != nil {
				t.Fatal(errDeadline)
			}
			payload := fmt.Sprintf(`{"type":"response.create","model":"gpt-5.4","input":[{"role":"user","content":%q}]}`, strings.Repeat("x", 200<<10))
			if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(payload)); errWrite != nil {
				t.Fatal(errWrite)
			}
			for {
				_, message, errRead := conn.ReadMessage()
				if errRead != nil {
					t.Fatal(errRead)
				}
				if !strings.Contains(string(message), `"id":"success"`) {
					t.Fatalf("unexpected event before success: %s", message)
				}
				if strings.Contains(string(message), `"type":"response.completed"`) {
					break
				}
			}
			if attempts.Load() != 4 {
				t.Fatalf("upstream attempts=%d, want 4", attempts.Load())
			}
		})
	}
}

func codexOutageProxy(t *testing.T, upstreamHandler http.HandlerFunc) (*httptest.Server, *cliproxyauth.Manager) {
	t.Helper()
	return codexOutageProxyWithConfig(t, &config.Config{}, false, upstreamHandler)
}

func codexOutageProxyWithConfig(t *testing.T, cfg *config.Config, upstreamWebsocket bool, upstreamHandler http.HandlerFunc) (*httptest.Server, *cliproxyauth.Manager) {
	t.Helper()
	upstream := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstream.Close)
	const authID = "codex-outage"
	const model = "gpt-5.4"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, time.Second, 0)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
	attributes := map[string]string{"base_url": upstream.URL, "api_key": "test"}
	if upstreamWebsocket {
		attributes["websockets"] = "true"
	}
	if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{
		ID: authID, Provider: "codex", Status: cliproxyauth.StatusActive,
		Attributes: attributes,
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	router := gin.New()
	handler := openaihandlers.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
	router.POST("/v1/responses", handler.Responses)
	router.GET("/v1/responses", handler.ResponsesWebsocket)
	proxy := httptest.NewServer(router)
	t.Cleanup(proxy.Close)
	return proxy, manager
}

func codexOutageRequest(t *testing.T, proxy *httptest.Server) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/v1/responses",
		strings.NewReader(`{"model":"gpt-5.4","input":"hello","stream":true}`))
	if errReq != nil {
		cancel()
		t.Fatal(errReq)
	}
	req.Header.Set("Content-Type", "application/json")
	response, errDo := proxy.Client().Do(req)
	if errDo != nil {
		cancel()
		t.Fatal(errDo)
	}
	return response, cancel
}
