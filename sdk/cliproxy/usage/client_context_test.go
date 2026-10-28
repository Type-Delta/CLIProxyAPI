package usage

import (
	"context"
	"testing"
)

func TestCancellationCauseKeepsFirstInitiator(t *testing.T) {
	for _, internal := range []string{"none", "plain", "typed"} {
		t.Run(internal, func(t *testing.T) {
			client, closeClient := context.WithCancel(context.Background())
			defer closeClient()
			root := WithClientRequestContext(context.Background(), client)
			execution, cancel := context.WithCancelCause(root)
			defer cancel(ErrInternalCancellation)
			switch internal {
			case "plain":
				cancel(context.Canceled)
			case "typed":
				cancel(ErrInternalCancellation)
			}
			closeClient()
			<-root.Done()
			<-execution.Done()
			if got := ClientRequestCanceled(execution); got != (internal == "none") {
				t.Fatalf("client=%v cause=%v", got, context.Cause(execution))
			}
		})
	}
}

func TestClientContextAsExecutionParent(t *testing.T) {
	client, cancel := context.WithCancel(context.Background())
	defer cancel()
	execution := WithClientRequestContext(client, client)
	cancel()
	<-execution.Done()
	if !ClientRequestCanceled(execution) {
		t.Fatalf("cause=%v", context.Cause(execution))
	}
}
