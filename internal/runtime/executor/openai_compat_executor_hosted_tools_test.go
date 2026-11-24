package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOpenAICompatExecutorRejectsHostedWebSearchBeforeUpstream(t *testing.T) {
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "provider-key"}}
	for _, mode := range []string{"execute", "stream", "count"} {
		t.Run(mode, func(t *testing.T) {
			req := cliproxyexecutor.Request{Model: "test-model", Payload: []byte(`{"input":"search","tools":[{"type":"web_search"},{"type":"function","name":"read_file","parameters":{"type":"object"}}],"tool_choice":{"type":"web_search"}}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
			var err error
			switch mode {
			case "stream":
				_, err = executor.ExecuteStream(context.Background(), auth, req, opts)
			case "count":
				_, err = executor.CountTokens(context.Background(), auth, req, opts)
			default:
				_, err = executor.Execute(context.Background(), auth, req, opts)
			}
			if err == nil || clienterror.HTTPStatusFromError(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "web_search") {
				t.Fatalf("error = %v, want a named 400 rejection", err)
			}
			if scoped, ok := err.(interface{ IsRequestScoped() bool }); !ok || !scoped.IsRequestScoped() {
				t.Fatalf("error = %T, want request-scoped rejection", err)
			}
		})
	}
	if calls := upstreamCalls.Load(); calls != 0 {
		t.Fatalf("upstream received %d unsupported requests", calls)
	}
}
