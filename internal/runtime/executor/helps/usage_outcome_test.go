package helps

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type outcomeTap func(usage.Record)

func (f outcomeTap) HandleUsage(_ context.Context, r usage.Record) { f(r) }

func TestReporterTerminalOutcomes(t *testing.T) {
	records := make(chan usage.Record, 10)
	unregister, err := usage.RegisterSanitizerTapNamedPlugin("outcome-regression", outcomeTap(func(r usage.Record) { records <- r }))
	if err != nil {
		t.Fatal(err)
	}
	defer unregister(context.Background())
	cases := []struct {
		name                string
		clientDone, success bool
		terminal            error
		want, hop           string
	}{
		{"client disconnect", true, false, context.Canceled, "canceled", ""},
		{"http2 body closure on disconnect", true, false, errors.New("http2: response body closed"), "canceled", ""},
		{"internal cancel", false, false, context.Canceled, "failed", "cpa_to_client"},
		{"success cleanup", false, true, nil, "succeeded", ""},
		{"provider failure then cancel", true, false, errors.New("upstream stream returned an error payload"), "failed", "provider_to_cpa"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, cancelClient := context.WithCancel(context.Background())
			defer cancelClient()
			ctx, cancelCause := context.WithCancelCause(usage.WithClientRequestContext(context.Background(), client))
			cancelExecution := func() { cancelCause(usage.ErrInternalCancellation) }
			defer cancelExecution()
			reporter := NewUsageReporter(ctx, "openai-compat", "fixture", nil)
			reporter.StartUpstreamTiming()
			reporter.RecordUpstreamStatus(200)
			if tc.clientDone {
				cancelClient()
				<-ctx.Done()
			}
			if tc.name == "internal cancel" {
				cancelExecution()
			}
			if tc.success {
				reporter.Publish(ctx, usage.Detail{InputTokens: 10, OutputTokens: 20, TotalTokens: 30})
				cancelExecution()
				reporter.PublishFailure(ctx, context.Canceled)
			} else {
				reporter.PublishFailureWithDetail(ctx, usage.Detail{InputTokens: 10, OutputTokens: 20, TotalTokens: 30}, tc.terminal)
				cancelExecution()
				reporter.PublishFailure(ctx, context.Canceled)
			}
			select {
			case got := <-records:
				if got.Outcome != tc.want || got.FailureHop != tc.hop || got.Detail.OutputTokens != 20 {
					t.Fatalf("record=%+v", got)
				}
				if tc.name == "internal cancel" && got.CancellationSource != "internal" {
					t.Fatalf("internal source=%s", got.CancellationSource)
				}
				if tc.want == "canceled" && got.CancellationSource != "client" {
					t.Fatalf("source=%s", got.CancellationSource)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no record")
			}
		})
	}
}
