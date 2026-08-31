package cliproxy

import (
	"context"
	"testing"
	"time"
)

func TestDeferredShutdownStartsTimeoutWhenInvoked(t *testing.T) {
	const shutdownTimeout = 200 * time.Millisecond

	shutdownCalled := false
	deferred := deferredShutdown(shutdownTimeout, func(ctx context.Context) error {
		shutdownCalled = true
		if err := ctx.Err(); err != nil {
			t.Fatalf("shutdown context already expired: %v", err)
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("shutdown context has no deadline")
		}
		if remaining := time.Until(deadline); remaining < shutdownTimeout/2 {
			t.Fatalf("shutdown timeout started before shutdown: remaining %v, want at least %v", remaining, shutdownTimeout/2)
		}
		return nil
	})

	time.Sleep(shutdownTimeout + 50*time.Millisecond)
	deferred()

	if !shutdownCalled {
		t.Fatal("deferred shutdown was not called")
	}
}
