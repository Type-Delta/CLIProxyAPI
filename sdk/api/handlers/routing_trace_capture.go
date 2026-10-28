package handlers

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func withAnalyticsTraceCapture(ctx context.Context) context.Context {
	if ctx == nil || !usage.TraceCaptureEnabled("cpauk-analytics") {
		return ctx
	}
	ctx, _ = routing.WithTraceHolder(ctx)
	return ctx
}
