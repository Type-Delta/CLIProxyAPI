package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	authsdk "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	execsdk "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	translator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCodexWebsocketGenerationSurvivesBlockedConsumer(t *testing.T) {
	for _, session := range []bool{false, true} {
		t.Run(map[bool]string{false: "sessionless", true: "session"}[session], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			records := make(chan usage.Record, 1)
			unregister, err := usage.RegisterAccountingNamedPlugin("generation-test", generationRecordCapture{records})
			if err != nil {
				t.Fatal(err)
			}
			defer unregister(ctx)
			sent := make(chan struct{})
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err = conn.ReadMessage(); err != nil {
					return
				}
				emit := func(s string) {
					if err := conn.WriteMessage(websocket.TextMessage, []byte(s)); err != nil {
						t.Error(err)
					}
				}
				for i := 0; i < 8; i++ {
					emit(`{"type":"response.created","response":{"id":"test","status":"in_progress","output":[]}}`)
				}
				emit(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}`)
				time.Sleep(150 * time.Millisecond)
				emit(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":" world"}`)
				emit(`{"type":"response.completed","response":{"id":"test","model":"generation-test","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":113,"total_tokens":123}}}`)
				close(sent)
				<-ctx.Done()
			}))
			defer server.Close()
			executor := NewCodexWebsocketsExecutor(&config.Config{})
			auth := &authsdk.Auth{Provider: "codex", Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
			opts := execsdk.Options{SourceFormat: translator.FromString("codex"), Stream: true}
			if session {
				opts.Metadata = map[string]any{execsdk.ExecutionSessionMetadataKey: "generation-ws-test"}
				defer executor.CloseExecutionSession("generation-ws-test")
			}
			resp, err := executor.ExecuteStream(ctx, auth, execsdk.Request{Model: "generation-test", Payload: []byte(`{"model":"generation-test","input":"hello"}`)}, opts)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			<-sent
			var output strings.Builder
			for chunk := range resp.Chunks {
				if chunk.Err != nil {
					cancel()
					t.Fatal(chunk.Err)
				}
				output.Write(chunk.Payload)
			}
			record := <-records
			cancel()
			if record.GenerationTime == nil || *record.GenerationTime < 120*time.Millisecond || *record.GenerationTime > 500*time.Millisecond {
				t.Fatalf("generation=%v; expected 150ms socket arrival interval", record.GenerationTime)
			}
			if record.FirstTokenLatency == nil || !strings.Contains(output.String(), "hello") {
				t.Fatal("missing token observation or payload")
			}
		})
	}
}

func TestCodexWebsocketQueuePressurePreservesPayloadAndMarksTiming(t *testing.T) {
	ch := make(chan codexWebsocketRead, 2)
	done := make(chan struct{})
	budget := newCodexWebsocketReadBudget()
	unreliable := false
	payload := make([]byte, 1024*1024)
	if !enqueueCodexWebsocketRead(ch, done, codexWebsocketRead{payload: payload}, budget, &unreliable) {
		t.Fatal("initial enqueue failed")
	}
	finished := make(chan bool, 1)
	go func() {
		finished <- enqueueCodexWebsocketRead(ch, done, codexWebsocketRead{payload: []byte("next")}, budget, &unreliable)
	}()
	first := <-ch
	// The first payload consumes the byte budget until the consumer releases it.
	first.budget.release(len(first.payload))
	if !<-finished {
		t.Fatal("payload dropped")
	}
	second := <-ch
	if string(second.payload) != "next" {
		t.Fatal("payload changed")
	}
	second.budget.release(len(second.payload))
	// Exercise deterministic queue saturation independently of scheduling above.
	full := make(chan codexWebsocketRead, 1)
	full <- codexWebsocketRead{}
	canceled := make(chan struct{})
	close(canceled)
	unreliable = false
	if enqueueCodexWebsocketRead(full, canceled, codexWebsocketRead{payload: []byte("held")}, budget, &unreliable) {
		t.Fatal("expected canceled blocked enqueue")
	}
	if !unreliable {
		t.Fatal("saturation did not invalidate timing")
	}
	if budget.bytes != 0 {
		t.Fatal("canceled payload budget leaked")
	}
}
