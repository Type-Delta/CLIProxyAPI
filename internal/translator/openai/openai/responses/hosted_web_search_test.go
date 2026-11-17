package responses

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
)

func TestResponsesToChatDropsHostedWebSearchDeclarations(t *testing.T) {
	for _, toolType := range []string{"web_search", "web_search_preview", "web_search_preview_2025_03_11"} {
		for _, declaration := range []string{
			`"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}},{"type":%q,"external_web_access":false}],"tool_choice":"auto"`,
			`"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}],"input":[{"type":"additional_tools","tools":[{"type":%q}]}]`,
			`"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}},{"type":"namespace","name":"hosted","tools":[{"type":%q}]}]`,
		} {
			t.Run(fmt.Sprintf("%s/%s", toolType, declaration), func(t *testing.T) {
				payload := []byte(`{"model":"test-model",` + fmt.Sprintf(declaration, toolType) + `}`)
				body, err := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("test-model", payload, true)
				if err != nil {
					t.Fatalf("optional hosted web search must not fail the request: %v", err)
				}
				tools := ojson.GetBytes(body, "tools").Array()
				if len(tools) != 1 || tools[0].Get("function.name").String() != "read_file" || strings.Contains(string(body), toolType) {
					t.Fatalf("want hosted search dropped and read_file kept: %s", body)
				}
			})
		}
	}
}

func TestResponsesToChatRejectsForcedHostedWebSearch(t *testing.T) {
	for _, toolType := range []string{"web_search", "web_search_preview"} {
		for _, stream := range []bool{false, true} {
			payload := []byte(`{"model":"test-model","tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}},{"type":"` + toolType + `"}],"tool_choice":{"type":"` + toolType + `"}}`)
			body, err := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("test-model", payload, stream)
			if err == nil || clienterror.HTTPStatusFromError(err) != http.StatusBadRequest || body != nil {
				t.Fatalf("%s stream=%v: error = %v, body = %s, want 400 without a payload", toolType, stream, err, body)
			}
			if scoped, ok := err.(interface{ IsRequestScoped() bool }); !ok || !scoped.IsRequestScoped() || !strings.Contains(err.Error(), toolType) {
				t.Fatalf("error = %v, want named request-scoped rejection", err)
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
