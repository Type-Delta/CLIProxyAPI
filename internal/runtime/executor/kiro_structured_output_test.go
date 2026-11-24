package executor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	tr "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestKiroStructuredOutputAndFinalWireReplacement(t *testing.T) {
	const schema = `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`
	for _, format := range []tr.Format{tr.FormatOpenAI, tr.FormatOpenAIResponse, tr.FormatClaude} {
		for _, stream := range []bool{false, true} {
			t.Run(string(format)+map[bool]string{false: "/buffered", true: "/streaming"}[stream], func(t *testing.T) {
				payload := `{"messages":[{"role":"user","content":"hero"}],"response_format":{"type":"json_schema","json_schema":{"schema":` + schema + `}}}`
				if format == tr.FormatOpenAIResponse {
					payload = `{"input":"hero","text":{"format":{"type":"json_schema","schema":` + schema + `}}}`
				}
				if format == tr.FormatClaude {
					payload = `{"messages":[{"role":"user","content":"hero"}],"output_config":{"format":{"type":"json_schema","schema":` + schema + `}}}`
				}
				received := make(chan []byte, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					received <- body
					writeKiroTestEvents(t, w, []kiroTestEvent{{name: "assistantResponseEvent", payload: map[string]any{"content": "ok"}}})
				}))
				defer upstream.Close()
				path := "conversationState.currentMessage.userInputMessage.content"
				cfg := &config.Config{Payload: config.PayloadConfig{Replace: []config.PayloadReplaceRule{
					{Models: []config.PayloadModelRule{{Name: "*"}}, Target: config.PayloadReplaceTarget{Role: "user", All: true}, Replacements: []config.PayloadReplacement{{"hero": "wrong-role"}}},
					{Models: []config.PayloadModelRule{{Name: "*"}}, Target: config.PayloadReplaceTarget{Paths: []string{path}}, Replacements: []config.PayloadReplacement{{"hero": "hero!"}}},
				}}}
				executor := NewKiroExecutor(cfg)
				req := ex.Request{Model: "claude-sonnet-5", Payload: []byte(payload)}
				opts := ex.Options{SourceFormat: format, Stream: stream}
				translated := kiroRequestPayload(req, opts)
				if ojson.GetBytes(translated, "output_config.format").Exists() {
					t.Fatal("Kiro retains native output format it cannot send")
				}
				if !strings.Contains(string(translated), `JSON Schema:`) {
					t.Fatalf("schema fallback missing: %s", translated)
				}
				if _, err := executor.CountTokens(t.Context(), kiroUsageTestAuth(), req, opts); err != nil {
					t.Fatal(err)
				}
				ctx := kiroUsageTestContext(upstream, t.Name())
				if stream {
					result, err := executor.ExecuteStream(ctx, kiroUsageTestAuth(), req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for c := range result.Chunks {
						if c.Err != nil {
							t.Fatal(c.Err)
						}
					}
				} else {
					if _, err := executor.Execute(ctx, kiroUsageTestAuth(), req, opts); err != nil {
						t.Fatal(err)
					}
				}
				wire := <-received
				text := ojson.GetBytes(wire, path).String()
				if !strings.Contains(text, schema) || !strings.Contains(text, "hero!") || strings.Contains(text, "hero!!") || strings.Contains(text, "wrong-role") {
					t.Fatalf("schema or final-wire replacement lost: %s", wire)
				}
			})
		}
	}
}
