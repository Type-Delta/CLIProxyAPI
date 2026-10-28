package usage

import (
	"context"
	"errors"
	"fmt"
)

// Cancellation causes distinguish the first client disconnect from execution
// cleanup. Context cancellation keeps its first cause even if the request closes later.
var (
	ErrClientDisconnected   = fmt.Errorf("client request disconnected: %w", context.Canceled)
	ErrInternalCancellation = fmt.Errorf("internal execution canceled: %w", context.Canceled)
)

// WithClientRequestContext bridges the downstream lifetime with an explicit cause.
// A caller canceling a derived execution context first remains internal/unknown.
func WithClientRequestContext(ctx, client context.Context) context.Context {
	if client == nil {
		return ctx
	}
	// When callers pass the HTTP context itself, its untyped native cancellation
	// must not beat the typed disconnect bridge.
	if ctx == client {
		ctx = context.WithoutCancel(ctx)
	}
	execution, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(client, func() { cancel(ErrClientDisconnected) })
	context.AfterFunc(execution, func() { stop() })
	return execution
}

func ClientRequestCanceled(ctx context.Context) bool {
	return ctx != nil && errors.Is(context.Cause(ctx), ErrClientDisconnected)
}
