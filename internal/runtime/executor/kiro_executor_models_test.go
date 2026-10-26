package executor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestKiroExecutorUsesWireModelIDs(t *testing.T) {
	for _, model := range []struct{ public, wire string }{
		{"claude-fable-5-1", "claude-fable-5.1"},
		{"claude-sonnet-5(high)", "claude-sonnet-5"},
		{"claude-opus-5-5(high)", "claude-opus-5.5"},
		{"claude-opus-5-20260724", "claude-opus-5"},
		{"claude-sonnet-4-6[1m]", "claude-sonnet-4.6"},
	} {
		for _, stream := range []bool{false, true} {
			for _, format := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", model.public, format, stream), func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, errRead := io.ReadAll(r.Body)
						if errRead != nil {
							t.Errorf("read Kiro request: %v", errRead)
							w.WriteHeader(http.StatusInternalServerError)
							return
						}
						for _, path := range []string{
							"conversationState.currentMessage.userInputMessage.modelId",
							"conversationState.history.0.userInputMessage.modelId",
						} {
							if got := gjson.GetBytes(body, path).String(); got != model.wire {
								t.Errorf("%s = %q, want %q", path, got, model.wire)
								http.Error(w, `{"message":"Invalid model ID. Please select a different model to continue.","reason":"INVALID_MODEL_ID"}`, http.StatusBadRequest)
								return
							}
						}
						writeKiroTestEvents(t, w, []kiroTestEvent{{name: "assistantResponseEvent", payload: map[string]any{"content": "OK"}}})
					}))
					t.Cleanup(server.Close)

					body := map[string]any{
						"model": model.public,
						"messages": []map[string]string{
							{"role": "user", "content": "hello"},
							{"role": "assistant", "content": "hi"},
							{"role": "user", "content": "Reply only OK."},
						},
					}
					if format == sdktranslator.FormatOpenAIResponse {
						body["input"] = body["messages"]
						delete(body, "messages")
					}
					payload, errMarshal := json.Marshal(body)
					if errMarshal != nil {
						t.Fatal(errMarshal)
					}
					executor := NewKiroExecutor(&config.Config{})
					ctx := kiroUsageTestContext(server, t.Name())
					req := cliproxyexecutor.Request{Model: model.public, Payload: payload}
					opts := cliproxyexecutor.Options{SourceFormat: format, Stream: stream}
					if stream {
						result, errStream := executor.ExecuteStream(ctx, kiroUsageTestAuth(), req, opts)
						if errStream != nil {
							t.Fatal(errStream)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, errExecute := executor.Execute(ctx, kiroUsageTestAuth(), req, opts); errExecute != nil {
						t.Fatal(errExecute)
					}
				})
			}
		}
	}
}
