package handlers

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestAuthUnavailableRecordCarriesLastRoutingTrace(t *testing.T) {
	records := make(chan coreusage.Record, 1)
	unregister, err := coreusage.RegisterSanitizerTapNamedPlugin("routing-trace-test", usagePluginFunc(func(_ context.Context, record coreusage.Record) {
		records <- record
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unregister(context.Background()) }()
	ctx, holder := routing.WithTraceHolder(context.Background())
	trace := &routing.Trace{Version: routing.TraceVersion, Mode: routing.ModeLocal, DecidedBy: routing.DecidedByStrategy, Pick: -1}
	holder.Set(trace)
	reportAuthUnavailable(ctx, &coreauth.Error{Code: "auth_unavailable", Message: "unavailable"}, []string{"openai"}, "model")
	select {
	case record := <-records:
		if record.RoutingTrace != trace {
			t.Fatalf("routing trace = %p, want %p", record.RoutingTrace, trace)
		}
	default:
		t.Fatal("auth-unavailable record was not published")
	}
}

type usagePluginFunc func(context.Context, coreusage.Record)

func (fn usagePluginFunc) HandleUsage(ctx context.Context, record coreusage.Record) { fn(ctx, record) }
