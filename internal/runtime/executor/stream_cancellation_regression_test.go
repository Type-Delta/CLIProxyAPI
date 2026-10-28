package executor

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	tr "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"testing"
	"time"
)

type regressionTap func(usage.Record)

func (f regressionTap) HandleUsage(_ context.Context, r usage.Record) { f(r) }

type qaBlockBody struct {
	prefix   io.Reader
	ctx      context.Context
	blocked  chan struct{}
	signaled bool
}

func (b *qaBlockBody) Read(p []byte) (int, error) {
	n, e := b.prefix.Read(p)
	if n > 0 {
		return n, nil
	}
	if e == io.EOF {
		if !b.signaled {
			b.signaled = true
			close(b.blocked)
		}
		<-b.ctx.Done()
		return 0, b.ctx.Err()
	}
	return n, e
}
func (b *qaBlockBody) Close() error { return nil }
func TestCanceledReadRecordsUsage(t *testing.T) {
	for _, provider := range []string{"codex", "openai", "claude"} {
		t.Run(provider, func(t *testing.T) {
			records := make(chan usage.Record, 10)
			unreg, e := usage.RegisterSanitizerTapNamedPlugin("regression-"+provider, regressionTap(func(r usage.Record) { records <- r }))
			if e != nil {
				t.Fatal(e)
			}
			defer unreg(context.Background())
			client, cancelClient := context.WithCancel(context.Background())
			defer cancelClient()
			ctx, cancel := context.WithCancel(usage.WithClientRequestContext(context.Background(), client))
			defer cancel()
			blocked := make(chan struct{})
			prefix := ""
			format := tr.FormatOpenAI
			model := "fixture"
			switch provider {
			case "codex":
				format = tr.FormatOpenAIResponse
				model = "gpt-5.4"
				prefix = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"gpt-5.4\",\"usage\":{\"input_tokens\":10,\"output_tokens\":3,\"total_tokens\":13}}}\n\n"
			case "openai":
				prefix = "data: {\"id\":\"c\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3,\"total_tokens\":13}}\n\n"
			case "claude":
				format = tr.FormatClaude
				model = "claude-sonnet-4-5"
				prefix = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"claude-sonnet-4-5\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n"
			}
			transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &qaBlockBody{prefix: strings.NewReader(prefix), ctx: r.Context(), blocked: blocked}, Request: r}, nil
			})
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(transport))
			a := &auth.Auth{Provider: provider, Attributes: map[string]string{"api_key": "qa-local", "base_url": "https://fixture.invalid/v1"}}
			payload := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"input":"hi","max_tokens":20}`)
			opts := ex.Options{SourceFormat: format, Stream: true}
			var stream *ex.StreamResult
			switch provider {
			case "codex":
				stream, e = NewCodexExecutor(&config.Config{}).ExecuteStream(ctx, a, ex.Request{Model: model, Payload: payload}, opts)
			case "openai":
				stream, e = NewOpenAICompatExecutor("openai", &config.Config{}).ExecuteStream(ctx, a, ex.Request{Model: model, Payload: payload}, opts)
			case "claude":
				stream, e = NewClaudeExecutor(&config.Config{}).ExecuteStream(ctx, a, ex.Request{Model: model, Payload: payload}, opts)
			}
			if e != nil {
				t.Fatal(e)
			}
			done := make(chan struct{})
			consumed := make(chan struct{})
			go func() {
				once := false
				for chunk := range stream.Chunks {
					if !once && len(chunk.Payload) > 0 {
						once = true
						close(consumed)
					}
				}
				close(done)
			}()
			select {
			case <-consumed:
			case <-time.After(5 * time.Second):
				t.Fatal("no initial payload")
			}
			select {
			case <-blocked:
			case <-time.After(5 * time.Second):
				t.Fatal("not reading upstream")
			}
			cancelClient()
			<-ctx.Done()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("stream leaked")
			}
			select {
			case r := <-records:
				t.Logf("provider=%s outcome=%s source=%s tokens=%+v", provider, r.Outcome, r.CancellationSource, r.Detail)
				if r.Outcome != "canceled" {
					t.Errorf("want canceled, got %s", r.Outcome)
				}
				if (provider == "openai" || provider == "codex") && r.Detail.OutputTokens != 3 {
					t.Errorf("lost partial usage: want 3 output, got %d", r.Detail.OutputTokens)
				}
			default:
				t.Error("no usage event published for client cancellation while scanner blocked")
			}
		})
	}
}
func TestClaudeProviderErrorWinsDisconnect(t *testing.T) {
	for _, format := range []tr.Format{tr.FormatClaude, tr.FormatOpenAI} {
		t.Run(string(format), func(t *testing.T) {
			records := make(chan usage.Record, 10)
			unreg, e := usage.RegisterSanitizerTapNamedPlugin("regression-claude-error", regressionTap(func(r usage.Record) { records <- r }))
			if e != nil {
				t.Fatal(e)
			}
			defer unreg(context.Background())
			client, cancelClient := context.WithCancel(context.Background())
			defer cancelClient()
			ctx, cancel := context.WithCancel(usage.WithClientRequestContext(context.Background(), client))
			defer cancel()
			prefix := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"claude-sonnet-4-5\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n" + "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"server_is_overloaded\"}}\n\n"
			blocked := make(chan struct{})
			transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &qaBlockBody{prefix: strings.NewReader(prefix), ctx: r.Context(), blocked: blocked}, Request: r}, nil
			})
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(transport))
			a := &auth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "qa-local", "base_url": "https://fixture.invalid/v1"}}
			stream, err := NewClaudeExecutor(&config.Config{}).ExecuteStream(ctx, a, ex.Request{Model: "claude-sonnet-4-5", Payload: []byte(`{"model":"claude-sonnet-4-5","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`)}, ex.Options{SourceFormat: format, Stream: true})
			if err != nil {
				t.Fatal(err)
			}
			var recorded usage.Record
			select {
			case recorded = <-records:
			case <-time.After(5 * time.Second):
				t.Fatal("observed provider error was not published before forwarding")
			}

			cancelClient()
			<-ctx.Done()
			for range stream.Chunks {
			}
			r := recorded
			t.Logf("known provider overload then disconnect: outcome=%s source=%s hop=%s failure=%+v", r.Outcome, r.CancellationSource, r.FailureHop, r.Fail)
			if r.Outcome != "failed" || r.FailureHop != "provider_to_cpa" {
				t.Error("provider error hidden by later disconnect")
			}
			if r.Detail.InputTokens != 10 {
				t.Fatalf("lost partial Claude usage: %+v", r.Detail)
			}
			if len(records) != 0 {
				t.Fatal("terminal event was published more than once")
			}

		})
	}
}
