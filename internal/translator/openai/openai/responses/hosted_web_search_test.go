package responses

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
)

func TestResponsesToChatRejectsHostedWebSearch(t *testing.T) {
	for _, toolType := range []string{"web_search", "web_search_preview", "web_search_preview_2025_03_11"} {
		for _, declaration := range []string{
			`"tools":[{"type":%q}]`,
			`"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}},{"type":%q}],"tool_choice":"auto"`,
			`"input":[{"type":"additional_tools","tools":[{"type":%q}]}]`,
			`"input":[{"type":"tool_search_output","execution":"client","tools":[{"type":%q}]}]`,
			`"tools":[{"type":"namespace","name":"hosted","tools":[{"type":%q}]}]`,
			`"tool_choice":{"type":%q}`,
		} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", toolType, declaration, stream), func(t *testing.T) {
					payload := []byte(`{"model":"test-model",` + fmt.Sprintf(declaration, toolType) + `}`)
					body, err := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("test-model", payload, stream)
					if err == nil || clienterror.HTTPStatusFromError(err) != http.StatusBadRequest {
						t.Fatalf("error = %v, want 400 for hosted web search; body = %s", err, body)
					}
					if scoped, ok := err.(interface{ IsRequestScoped() bool }); !ok || !scoped.IsRequestScoped() {
						t.Fatalf("error = %T, want request-scoped rejection", err)
					}
					if !strings.Contains(err.Error(), toolType) || !strings.Contains(err.Error(), "Chat Completions") || body != nil {
						t.Fatalf("error = %v, body = %s, want named unsupported tool without a partial payload", err, body)
					}
				})
			}
		}
	}
}

func TestResponsesToChatPreservesWebSearchFunction(t *testing.T) {
	payload := []byte(`{"input":"search","tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"web_search"}}`)
	body, err := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("test-model", payload, false)
	if err != nil {
		t.Fatal(err)
	}
	if ojson.GetBytes(body, "tools.0.function.name").String() != "web_search" || ojson.GetBytes(body, "tool_choice.function.name").String() != "web_search" {
		t.Fatalf("caller-owned web_search function changed: %s", body)
	}
}
