package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type captureCodexLatencyUsage struct{ records chan usage.Record }

func (p *captureCodexLatencyUsage) HandleUsage(_ context.Context, record usage.Record) {
	if record.Model == "codex-latency-test" {
		p.records <- record
	}
}

func TestCodexLocalLatencyMeasuresEachReusedWebsocketRequest(t *testing.T) {
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		connections.Add(1)
		defer func() { _ = connection.Close() }()
		for index := 0; index < 2; index++ {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
			frames := []string{`{"type":"response.created","response":{"id":"test"}}`, `{"type":"response.output_text.delta","delta":"hello"}`, `{"type":"response.completed","response":{"id":"test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`}
			for _, frame := range frames {
				if err := connection.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
					t.Error(err)
					return
				}
			}
		}
		// Keep the connection alive until the executor closes its session.
		_, _, _ = connection.ReadMessage()
	}))
	defer server.Close()
	plugin := &captureCodexLatencyUsage{records: make(chan usage.Record, 2)}
	unregister, err := usage.RegisterPlugin(plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unregister(context.Background()) }()
	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	const session = "local-latency-reuse"
	defer exec.CloseExecutionSession(session)
	auth := &cliproxyauth.Auth{ID: "local-latency-auth", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	request := cliproxyexecutor.Request{Model: "codex-latency-test", Payload: []byte(`{"model":"codex-latency-test","input":"hello"}`)}
	options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: session}}
	for index := 0; index < 2; index++ {
		if _, err := exec.Execute(context.Background(), auth, request, options); err != nil {
			t.Fatal(err)
		}
		select {
		case record := <-plugin.records:
			if record.ProviderLatency == nil || record.FirstTokenLatency == nil || *record.ProviderLatency < 0 || *record.FirstTokenLatency < *record.ProviderLatency {
				t.Fatalf("request %d timing=%+v", index, record)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("usage record missing")
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("connections=%d, want reuse", connections.Load())
	}
}
