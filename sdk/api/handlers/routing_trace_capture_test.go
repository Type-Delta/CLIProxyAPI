package handlers

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type traceCaptureProbe struct{ active bool }

func (traceCaptureProbe) HandleUsage(context.Context, coreusage.Record) {}
func (p traceCaptureProbe) TraceCaptureActive() bool                    { return p.active }

func TestAnalyticsTraceHolderInstalledOnlyWhenCaptureIsActive(t *testing.T) {
	for _, active := range []bool{false, true} {
		unregister, err := coreusage.RegisterSanitizerTapNamedPlugin("cpauk-analytics", traceCaptureProbe{active: active})
		if err != nil {
			t.Fatal(err)
		}
		ctx := withAnalyticsTraceCapture(context.Background())
		if got := routing.HolderFromContext(ctx) != nil; got != active {
			t.Fatalf("holder present = %t, capture active = %t", got, active)
		}
		if err := unregister(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
