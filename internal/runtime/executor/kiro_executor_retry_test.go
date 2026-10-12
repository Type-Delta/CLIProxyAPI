package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestKiroExecutorRetryAfterHeader(t *testing.T) {
	tests := []struct {
		name       string
		retryAfter string
		wantRetry  *time.Duration
	}{
		{name: "seconds", retryAfter: "120", wantRetry: durationPointer(120 * time.Second)},
		{name: "absent"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				header := http.Header{"Content-Type": []string{"application/json"}}
				if test.retryAfter != "" {
					header.Set("Retry-After", test.retryAfter)
				}
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Status:     "429 Too Many Requests",
					Header:     header,
					Body:       io.NopCloser(strings.NewReader(`{"message":"limited"}`)),
					Request:    req,
				}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
			executor := NewKiroExecutor(nil)
			auth := &cliproxyauth.Auth{Provider: "kiro", Metadata: map[string]any{"access_token": "test-token"}}
			request := cliproxyexecutor.Request{
				Model:   "claude-sonnet-4-5",
				Payload: []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`),
			}
			for _, invoke := range []struct {
				name string
				run  func() error
			}{
				{
					name: "execute",
					run: func() error {
						_, errExecute := executor.Execute(ctx, auth, request, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
						return errExecute
					},
				},
				{
					name: "stream",
					run: func() error {
						_, errStream := executor.ExecuteStream(ctx, auth, request, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: true})
						return errStream
					},
				},
			} {
				t.Run(invoke.name, func(t *testing.T) {
					errUpstream := invoke.run()
					if errUpstream == nil {
						t.Fatal("expected Kiro upstream error")
					}
					retryable, ok := errUpstream.(interface{ RetryAfter() *time.Duration })
					if !ok {
						t.Fatalf("error %T does not expose RetryAfter", errUpstream)
					}
					got := retryable.RetryAfter()
					if test.wantRetry == nil {
						if got != nil {
							t.Fatalf("RetryAfter = %v, want nil", *got)
						}
						return
					}
					if got == nil || *got != *test.wantRetry {
						t.Fatalf("RetryAfter = %v, want %v", got, *test.wantRetry)
					}
				})
			}
		})
	}
}
