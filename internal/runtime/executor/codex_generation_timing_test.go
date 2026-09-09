package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	authsdk "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	execsdk "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	translator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type generationRecordCapture struct{ records chan usage.Record }

func (c generationRecordCapture) HandleUsage(_ context.Context, r usage.Record) {
	if r.Model == "generation-test" {
		c.records <- r
	}
}

func TestCodexGenerationMeasuresUpstreamDespiteBlockedConsumer(t *testing.T) {
	for _, mode := range []string{"stream", "bootstrap", "nonstream", "terminal-only"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			records := make(chan usage.Record, 1)
			unregister, err := usage.RegisterAccountingNamedPlugin("generation-test", generationRecordCapture{records})
			if err != nil {
				t.Fatal(err)
			}
			defer unregister(ctx)
			sent := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				emit := func(payload string) { _, _ = fmt.Fprintf(w, "data: %s\n\n", payload); w.(http.Flusher).Flush() }
				for i := 0; i < 8; i++ {
					emit(`{"type":"response.created","response":{"id":"test","status":"in_progress","output":[]}}`)
				}
				if mode != "terminal-only" {
					emit(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}`)
					time.Sleep(150 * time.Millisecond)
					emit(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":" world"}`)
				}
				emit(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_test","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello world","annotations":[]}]}}`)
				time.Sleep(50 * time.Millisecond)
				emit(`{"type":"response.completed","response":{"id":"test","model":"generation-test","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":113,"total_tokens":123}}}`)
				close(sent)
			}))
			defer server.Close()
			executor := NewCodexExecutor(&config.Config{Codex: config.CodexConfig{StreamBootstrapBuffering: mode == "bootstrap"}})
			auth := &authsdk.Auth{Provider: "codex", Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
			req := execsdk.Request{Model: "generation-test", Payload: []byte(`{"model":"generation-test","input":"hello"}`)}
			opts := execsdk.Options{SourceFormat: translator.FromString("openai-response"), Stream: mode != "nonstream"}
			var output strings.Builder
			if mode == "nonstream" {
				resp, err := executor.Execute(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				output.Write(resp.Payload)
			} else {
				resp, err := executor.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				<-sent
				for chunk := range resp.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					output.Write(chunk.Payload)
				}
			}
			record := <-records
			if record.Failed || record.FirstTokenLatency == nil || record.Detail.OutputTokens != 113 {
				t.Fatalf("missing successful observation: %+v", record)
			}
			if !strings.Contains(output.String(), "hello world") {
				t.Fatal("response content lost")
			}
			if mode == "terminal-only" {
				if record.GenerationTime != nil {
					t.Fatal("terminal snapshot invented duration")
				}
				return
			}
			if record.GenerationTime == nil || *record.GenerationTime < 120*time.Millisecond || *record.GenerationTime > 500*time.Millisecond {
				t.Fatalf("generation=%v expected actual 150ms upstream span", record.GenerationTime)
			}
			// A genuine short interval stays measurable; no duration or TPS cutoff is applied.
			if float64(record.Detail.OutputTokens)/record.GenerationTime.Seconds() < 200 {
				t.Fatal("fast generation rejected")
			}
		})
	}
}
