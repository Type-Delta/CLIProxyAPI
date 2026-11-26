package executor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestKiroResponsesLateSystemItemsRemainSeparate(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streaming"}[stream], func(t *testing.T) {
			received := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read request: %v", err)
				}
				received <- body
				writeKiroTestEvents(t, w, []kiroTestEvent{{name: "assistantResponseEvent", payload: map[string]any{"content": "ok"}}})
			}))
			defer server.Close()
			req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"model":"claude-opus-5","instructions":"base","input":[{"role":"user","content":"first"},{"role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"role":"system","content":"late system"},{"role":"developer","content":[{"type":"input_text","text":"late developer"}]},{"role":"user","content":"second"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: stream}
			exec := NewKiroExecutor(nil)
			ctx := kiroUsageTestContext(server, t.Name())
			if stream {
				result, err := exec.ExecuteStream(ctx, kiroUsageTestAuth(), req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else {
				if _, err := exec.Execute(ctx, kiroUsageTestAuth(), req, opts); err != nil {
					t.Fatal(err)
				}
			}
			wire := <-received
			var payload struct {
				ConversationState struct {
					History []struct {
						UserInputMessage struct {
							Content string `json:"content"`
						} `json:"userInputMessage"`
					} `json:"history"`
					CurrentMessage struct {
						UserInputMessage struct {
							Content string `json:"content"`
						} `json:"userInputMessage"`
					} `json:"currentMessage"`
				} `json:"conversationState"`
			}
			if err := json.Unmarshal(wire, &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload.ConversationState.History) == 0 {
				t.Fatalf("missing history: %s", wire)
			}
			system := payload.ConversationState.History[0].UserInputMessage.Content
			for _, text := range []string{"base", "late system", "late developer"} {
				if !strings.Contains(system, text) {
					t.Errorf("initial system content %q missing %q; wire: %s", system, text, wire)
				}
			}
			if got := payload.ConversationState.CurrentMessage.UserInputMessage.Content; got != "second" {
				t.Errorf("current user content = %q, want second", got)
			}
		})
	}
}

func TestKiroGeminiAndInteractionsUseSharedClaudeTranslation(t *testing.T) {
	const model = "claude-sonnet-4-6"
	for _, tc := range []struct {
		name, request, thinking string
		source                  sdktranslator.Format
	}{
		{"Gemini camel case", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":4096,"thinkingConfig":{"thinkingBudget":0}}}`, "disabled", sdktranslator.FormatGemini},
		{"Gemini snake case", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generation_config":{"max_output_tokens":4096,"thinking_config":{"thinking_budget":0}}}`, "disabled", sdktranslator.FormatGemini},
		{"Interactions", `{"input":"hello","generation_config":{"max_output_tokens":4096,"thinking_level":"auto"}}`, "adaptive", sdktranslator.FormatInteractions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(tc.request)}
			opts := cliproxyexecutor.Options{SourceFormat: tc.source}
			claude := kiroRequestPayload(req, opts)
			if maxTokens := ojson.GetBytes(claude, "max_tokens").Int(); maxTokens != 4096 ||
				ojson.GetBytes(claude, "thinking.type").String() != tc.thinking {
				t.Fatalf("Claude translation lost generation settings: %s", claude)
			}
			wire, err := buildKiroPayload(model, claude, "", "translation-fixture")
			if err != nil {
				t.Fatal(err)
			}
			if got := ojson.GetBytes(wire, "conversationState.currentMessage.userInputMessage.content").String(); got != "hello" {
				t.Fatalf("Kiro wire user content = %q, want hello", got)
			}
		})
	}
}
