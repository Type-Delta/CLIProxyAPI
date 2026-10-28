package api

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse/chtest"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/outbox"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestTelemetryRealServerConversation(t *testing.T) {
	h := chtest.Handle(t)
	db, err := h.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("CPA_TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		b, err := exec.Command("docker", "port", "cpa-test-clickhouse", "9000/tcp").Output()
		if err != nil {
			t.Fatal(err)
		}
		dsn = "clickhouse://" + strings.TrimSpace(strings.Split(string(b), "\n")[0])
	}
	for _, serverMode := range []bool{true, false} {
		t.Run(fmt.Sprint("server_mode_", serverMode), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("WRITABLE_PATH", dir)
			var calls atomic.Int32
			assistant := map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_read", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"AAA"}`}}}}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				b, _ := io.ReadAll(r.Body)
				if n == 3 {
					w.WriteHeader(403)
					fmt.Fprint(w, `{"error":{"message":"denied","type":"permission_error"}}`)
					return
				}
				var body struct {
					Stream bool `json:"stream"`
				}
				_ = json.Unmarshal(b, &body)
				if body.Stream {
					w.Header().Set("Content-Type", "text/event-stream")
					chunk, _ := json.Marshal(map[string]any{"id": "chat-smoke", "object": "chat.completion.chunk", "model": "smoke-model", "choices": []any{map[string]any{"index": 0, "delta": assistant, "finish_reason": nil}}})
					fmt.Fprintf(w, "data: %s\n\ndata: {\"id\":\"chat-smoke\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", chunk)
					w.(http.Flusher).Flush()
				} else {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "chat-smoke", "object": "chat.completion", "model": "smoke-model", "choices": []any{map[string]any{"index": 0, "message": assistant, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}})
				}
			}))
			defer up.Close()
			cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []config.APIKeyEntry{{Key: "smoke-client"}}, RequestLog: true}, ClickHouse: config.DefaultClickHouseConfig()}
			hashed, errHash := bcrypt.GenerateFromPassword([]byte("smoke-management"), bcrypt.MinCost)
			if errHash != nil {
				t.Fatal(errHash)
			}
			cfg.RemoteManagement.SecretKey = string(hashed)
			cfg.RemoteManagement.AllowRemote = true
			cfg.ClickHouse.Database = db.Database()
			if serverMode {
				cfg.ClickHouse.DSN = dsn
			}
			cfg.DisableCooling = true
			cfg.OpenAICompatibility = []config.OpenAICompatibility{{Name: "smoke-provider", BaseURL: up.URL + "/v1", Models: []config.OpenAICompatibilityModel{{Name: "smoke-model", Alias: "smoke-model"}}}}
			s := newTestServerWithConfig(t, cfg)
			if !s.applyAccessConfig(nil, cfg) {
				t.Fatal("access config")
			}
			manager := s.handlers.AuthManager
			manager.RegisterExecutor(executor.NewOpenAICompatExecutor("smoke-provider", cfg))
			cred := &auth.Auth{ID: "smoke-auth", Provider: "smoke-provider", Status: auth.StatusActive, Attributes: map[string]string{"base_url": up.URL + "/v1", "api_key": "mock-upstream"}}
			if _, err = manager.Register(context.Background(), cred); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(cred.ID, cred.Provider, []*registry.ModelInfo{{ID: "smoke-model"}})
			defer registry.GetGlobalRegistry().UnregisterClient(cred.ID)
			live := httptest.NewServer(s.engine)
			s.server = live.Config
			defer live.Close()
			defer s.Stop(context.Background())
			send := func(path, key string, body any) (int, []byte) {
				t.Helper()
				b, _ := json.Marshal(body)
				req, _ := http.NewRequest("POST", live.URL+path, strings.NewReader(string(b)))
				req.Header.Set("Authorization", "Bearer "+key)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Session_id", "xb12345")
				resp, e := http.DefaultClient.Do(req)
				if e != nil {
					t.Fatal(e)
				}
				defer resp.Body.Close()
				data, e := io.ReadAll(resp.Body)
				if e != nil {
					t.Fatal(e)
				}
				return resp.StatusCode, data
			}
			query := func(sql string, params any) map[string]any {
				t.Helper()
				status, b := send("/v8/management/telemetry/query", "smoke-management", map[string]any{"sql": sql, "params": params})
				if status != 200 {
					t.Fatalf("SQL %s: %d %s", sql, status, b)
				}
				var result map[string]any
				if err := json.Unmarshal(b, &result); err != nil {
					t.Fatal(err)
				}
				t.Logf("SQL %s => %s", sql, b)
				return result
			}
			for _, key := range []string{"", "smoke-client"} {
				status, b := send("/v8/management/telemetry/query", key, map[string]string{"sql": "SELECT 1"})
				if status != 401 {
					t.Fatalf("management auth %q: %d %s", key, status, b)
				}
			}
			messages := []any{map[string]any{"role": "user", "content": "read AAA"}}
			for turn := 0; turn < 4; turn++ {
				if turn == 1 {
					messages = append(messages, assistant, map[string]any{"role": "tool", "tool_call_id": "call_read", "content": "skill AAA contents"})
				}
				status, b := send("/v1/chat/completions", "smoke-client", map[string]any{"model": "smoke-model", "messages": messages, "stream": turn == 1})
				expected := 200
				if turn == 2 {
					expected = 403
				}
				if status != expected {
					t.Fatalf("turn %d: %d %s", turn, status, b)
				}
			}
			if calls.Load() != 4 {
				t.Fatalf("upstream calls %d", calls.Load())
			}
			// Stop immediately after response completion: shutdown must flush and drain captures.
			if err = s.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			logs := filepath.Join(dir, "logs")
			paths, err := outbox.List(logs)
			if err != nil {
				t.Fatal(err)
			}
			if len(paths) != 0 {
				t.Fatalf("shutdown left %d pending records", len(paths))
			}
			if !serverMode {
				if _, err = os.Stat(filepath.Join(logs, "telemetry-outbox")); !os.IsNotExist(err) {
					t.Fatalf("plain mode outbox: %v", err)
				}
				files, _ := filepath.Glob(filepath.Join(logs, "*.log"))
				if len(files) != 4 {
					t.Fatalf("plain logs %v", files)
				}
				for _, p := range files {
					b, _ := os.ReadFile(p)
					if !strings.Contains(string(b), "=== REQUEST INFO ===") || !strings.Contains(string(b), "=== RESPONSE ===") {
						t.Fatalf("legacy format %s", b)
					}
				}
				return
			}
			s.mgmt.SetClickHouse(h)
			defer h.CloseAgent()
			// The independent verification handle survives server shutdown.
			counts := map[string]uint64{}
			for _, table := range []string{"requests", "attempts", "request_contexts", "transcript_entries", "transcript_content"} {
				var n uint64
				if err = db.Conn().QueryRow(context.Background(), "SELECT count() FROM `"+db.Database()+"`."+table).Scan(&n); err != nil {
					t.Fatal(err)
				}
				counts[table] = n
				if n == 0 {
					t.Fatal(table + " empty")
				}
			}
			t.Logf("table counts: %v", counts)
			if counts["requests"] != 4 || counts["attempts"] != 4 || counts["request_contexts"] != 8 {
				t.Fatal(counts)
			}
			var bad uint64
			if err = db.Conn().QueryRow(context.Background(), "SELECT count() FROM `"+db.Database()+"`.requests WHERE proxy_request_id='' OR actor_key_id != ?", config.APIKeyID("smoke-client")).Scan(&bad); err != nil || bad != 0 {
				t.Fatalf("missing identity %d %v", bad, err)
			}
			// The same three-element history and assistant response must share their chain.
			if counts["transcript_entries"] != 4 {
				t.Fatalf("history dedupe: %v", counts)
			}
			// Use a fresh live listener because Stop closed the original HTTP server.
			live2 := httptest.NewServer(s.engine)
			defer live2.Close()
			live = live2
			r := query(`SELECT r.started_at, r.request_id, r.status AS client_status, a.attempt_index, a.status AS upstream_status, a.provider, a.url, a.request_headers, a.response_headers, a.request_body_raw FROM requests AS r LEFT JOIN attempts AS a USING (request_id) WHERE r.session_id = {session:String} AND (r.status = 403 OR a.status = 403) ORDER BY r.started_at, a.attempt_index`, map[string]string{"session": "codex:xb12345"})
			if r["row_count"] != float64(1) {
				t.Fatal(r)
			}
			r = query(`SELECT uniqExact(e.entry_id) AS skill_occurrences FROM transcript_entries AS e INNER JOIN transcript_content AS c ON e.content_id = c.content_id WHERE c.text LIKE '%skill AAA contents%' AND e.occurred_at >= toStartOfMonth(now()) AND e.occurred_at < addMonths(toStartOfMonth(now()), 1)`, nil)
			rows := r["rows"].([]any)
			if rows[0].([]any)[0] != float64(1) {
				t.Fatal(r)
			}
		})
	}
}

func TestTelemetryHomeLoggerReload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WRITABLE_PATH", dir)
	cfg := &config.Config{ClickHouse: config.DefaultClickHouseConfig()}
	cfg.ClickHouse.DSN = "clickhouse://127.0.0.1:1"
	cfg.RequestLog = true
	s := newTestServerWithConfig(t, cfg)
	defer s.Stop(context.Background())
	if _, ok := s.requestLogger.(*logging.TelemetryRequestLogger); !ok {
		t.Fatalf("%T", s.requestLogger)
	}
	s.engine.POST("/v1/i1-home", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	capture := func() {
		t.Helper()
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, httptest.NewRequest("POST", "/v1/i1-home", nil))
		if w.Code != 200 && w.Code != 503 {
			t.Fatalf("capture request: %d", w.Code)
		}
	}
	capture()
	paths, err := outbox.List(filepath.Join(dir, "logs"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("initial outbox %d: %v", len(paths), err)
	}
	next := cfg.CloneForRuntime()
	next.Home.Enabled = true
	s.UpdateClients(next)
	if _, ok := s.requestLogger.(*logging.FileRequestLogger); !ok {
		t.Fatalf("Home logger %T", s.requestLogger)
	}
	capture()
	paths, err = outbox.List(filepath.Join(dir, "logs"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("Home wrote outbox: %d %v", len(paths), err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "logs", "*.log"))
	if len(files) != 0 {
		t.Fatalf("Home wrote files: %v", files)
	}
	next = next.CloneForRuntime()
	next.Home.Enabled = false
	s.UpdateClients(next)
	if _, ok := s.requestLogger.(*logging.TelemetryRequestLogger); !ok {
		t.Fatalf("restored logger %T", s.requestLogger)
	}
	capture()
	paths, err = outbox.List(filepath.Join(dir, "logs"))
	if err != nil || len(paths) != 2 {
		t.Fatalf("restored outbox %d: %v", len(paths), err)
	}
	if err = s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	paths, err = outbox.List(filepath.Join(dir, "logs"))
	if err != nil || len(paths) != 2 {
		t.Fatalf("unavailable shutdown lost outbox: %d %v", len(paths), err)
	}
}

// Poll real ClickHouse state with a bounded deadline, rather than assuming driver cancellation worked.
func waitTelemetryCondition(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if fn() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("condition did not become true")
		case <-tick.C:
		}
	}
}
