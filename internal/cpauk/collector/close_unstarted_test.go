package collector

import (
	"context"
	"testing"
	"time"
)

func TestR2UnstartedCollectorCloseDoesNotHoldOwnership(t *testing.T) {
	c, err := New(&recordingWriter{}, testOptions(4, 1))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatalf("collector without a worker failed to close: %v", err)
	}
}
