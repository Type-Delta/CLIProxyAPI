package helps

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageReporterRetainsKnownClientStreamMode(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		ctx                   context.Context
		set                   bool
		wantKnown, wantStream bool
	}{
		{"unknown", context.Background(), false, false, false},
		{"explicit-false", usage.WithStream(context.Background(), false), false, true, false},
		{"explicit-true", usage.WithStream(context.Background(), true), false, true, true},
		{"setter-false", context.Background(), true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewUsageReporter(tc.ctx, "test", "test", nil)
			if tc.set {
				r.SetStream(false)
			}
			record := r.buildRecord(usage.Detail{}, false)
			known := record.StreamKnown
			if known != tc.wantKnown || record.Stream != tc.wantStream {
				t.Fatalf("stream mode known=%v stream=%v, want known=%v stream=%v", known, record.Stream, tc.wantKnown, tc.wantStream)
			}
		})
	}
}
