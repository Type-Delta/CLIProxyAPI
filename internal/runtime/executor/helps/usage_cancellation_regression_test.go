package helps

import (
	"context"
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"testing"
)

func TestInternalCancelThenRequestCleanup(t *testing.T) {
	records := make(chan usage.Record, 1)
	unreg, err := usage.RegisterSanitizerTapNamedPlugin("regression-internal-cause", outcomeTap(func(r usage.Record) { records <- r }))
	if err != nil {
		t.Fatal(err)
	}
	defer unreg(context.Background())
	client, closeRequest := context.WithCancel(context.Background())
	defer closeRequest()
	ctx, cancelInternal := context.WithCancelCause(usage.WithClientRequestContext(context.Background(), client))
	reporter := NewUsageReporter(ctx, "codex", "fixture", nil)
	reporter.StartUpstreamTiming()
	reporter.RecordUpstreamStatus(200)
	cancelInternal(errors.New("internal execution drain"))
	closeRequest()
	reporter.PublishFailure(ctx, ctx.Err())
	r := <-records
	t.Logf("execution cause=%v outcome=%s source=%s hop=%s", context.Cause(ctx), r.Outcome, r.CancellationSource, r.FailureHop)
	if r.CancellationSource == "client" {
		t.Error("internal cancellation attributed to client merely because request is closed at publication")
	}
}

func TestPublicationContextKeepsInternalChildCause(t *testing.T) {
	records := make(chan usage.Record, 1)
	unregister, err := usage.RegisterSanitizerTapNamedPlugin("internal-child-cause", outcomeTap(func(r usage.Record) { records <- r }))
	if err != nil {
		t.Fatal(err)
	}
	defer unregister(context.Background())
	client, closeClient := context.WithCancel(context.Background())
	defer closeClient()
	root := usage.WithClientRequestContext(context.Background(), client)
	reporter := NewUsageReporter(root, "codex", "fixture", nil)
	reporter.StartUpstreamTiming()
	reporter.RecordUpstreamStatus(200)
	child, cancelInternal := context.WithCancelCause(root)
	cancelInternal(usage.ErrInternalCancellation)
	closeClient()
	<-root.Done()
	reporter.PublishFailure(child, child.Err())
	got := <-records
	if got.Outcome != "failed" || got.CancellationSource != "internal" || got.FailureHop != "cpa_to_client" {
		t.Fatalf("record=%+v", got)
	}
}
