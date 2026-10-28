//go:build !race

package outbox

import (
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
)

// Race instrumentation deliberately drops sync.Pool returns, so allocation
// budgets are measured without instrumentation. Functional reuse is race-tested.
func TestRepeatedRecordsReuseCompressionState(t *testing.T) {
	gcPercent := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gcPercent)
	dir := t.TempDir()
	r := &record.Request{RequestID: "reuse", StartedAt: time.Unix(1, 0), Body: []byte("small payload")}
	if err := Write(dir, r); err != nil {
		t.Fatal(err)
	}
	paths, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Warm the real write/read paths. Pooled compression state should make tiny
	// requests cheap rather than allocating fresh megabyte-sized codec state.
	for n := 0; n < 3; n++ {
		if err = Write(dir, r); err != nil {
			t.Fatal(err)
		}
		if _, err = Read(paths[0]); err != nil {
			t.Fatal(err)
		}
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for n := 0; n < 5; n++ {
		if err = Write(dir, r); err != nil {
			t.Fatal(err)
		}
		if _, err = Read(paths[0]); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
		t.Fatalf("tiny records allocated %d bytes of compression state", allocated)
	}
}
