package executor

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type canceledDirectImageBody struct {
	ctx    context.Context
	cancel context.CancelFunc
	sent   bool
}

func (b *canceledDirectImageBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, io.EOF
	}
	b.sent = true
	b.cancel()
	<-b.ctx.Done()
	return copy(p, []byte("data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"AA==\",\"usage\":{\"input_tokens\":5,\"output_tokens\":7,\"total_tokens\":12}}\n\n")), nil
}
func (b *canceledDirectImageBody) Close() error { return nil }
func TestCodexDirectImageClientCancellationRetainsObservedUsage(t *testing.T) {
	records := make(chan usage.Record, 4)
	unregister, err := usage.RegisterSanitizerTapNamedPlugin(t.Name(), partialUsageTap(func(r usage.Record) { records <- r }))
	if err != nil {
		t.Fatal(err)
	}
	defer unregister(context.Background())
	client, cancelClient := context.WithCancel(t.Context())
	defer cancelClient()
	execution := usage.WithClientRequestContext(t.Context(), client)
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &canceledDirectImageBody{ctx: execution, cancel: cancelClient}}, nil
	})
	ctx := context.WithValue(execution, "cliproxy.roundtripper", transport)
	result, err := NewCodexExecutor(&config.Config{}).ExecuteStream(ctx, codexCompressionAuth(), cliproxyexecutor.Request{Model: "gpt-image-2.6", Payload: []byte(`{"model":"gpt-image-2.6","prompt":"cat","stream":true}`)}, codexOpenAIImageTestOptions(codexImagesGenerationsPath, true))
	if err != nil {
		t.Fatal(err)
	}
	// Leave delivery unconsumed until cancellation has closed the producer.
	select {
	case r := <-records:
		t.Logf("record outcome=%q failed=%t cancellation=%q tokens=%d/%d/%d", r.Outcome, r.Failed, r.CancellationSource, r.Detail.InputTokens, r.Detail.OutputTokens, r.Detail.TotalTokens)
		if r.Detail.InputTokens != 5 || r.Detail.OutputTokens != 7 || r.Detail.TotalTokens != 12 {
			t.Fatalf("tokens=%d/%d/%d; want 5/7/12", r.Detail.InputTokens, r.Detail.OutputTokens, r.Detail.TotalTokens)
		}
		if r.Outcome != "canceled" || r.CancellationSource != "client" {
			t.Fatalf("want canceled/client after downstream cancellation, got %q/%q", r.Outcome, r.CancellationSource)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no cancellation usage record")
	}
	for range result.Chunks {
	}
	select {
	case extra := <-records:
		t.Fatalf("unexpected duplicate usage record: %+v", extra)
	default:
	}
}
