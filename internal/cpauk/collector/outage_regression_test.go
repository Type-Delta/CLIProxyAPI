package collector

import (
	"context"
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"sync/atomic"
	"testing"
)

type outageWriter struct {
	down  atomic.Bool
	calls atomic.Int64
	wrote atomic.Int64
}

func (w *outageWriter) WriteBatch(ctx context.Context, e []model.Event) error {
	w.calls.Add(1)
	if w.down.Load() {
		return errors.New("database unavailable")
	}
	w.wrote.Add(int64(len(e)))
	return nil
}
func TestOutageRetainsBoundedIntakeAndRecovers(t *testing.T) {
	w := &outageWriter{}
	w.down.Store(true)
	opts := testOptions(4, 1)
	opts.FailureThreshold = 1
	c, err := New(w, opts)
	if err != nil {
		t.Fatal(err)
	}
	c.Start()
	defer c.Close(context.Background())
	if !c.Enqueue(c.Generation(), validEvent()) {
		t.Fatal("initial enqueue")
	}
	waitFor(t, func() bool { return w.calls.Load() > 0 })
	if !c.Enqueue(c.Generation(), validEvent()) {
		w.down.Store(false)
		t.Fatal("outage detached bounded intake")
	}
	w.down.Store(false)
	waitFor(t, func() bool { return w.wrote.Load() == 2 })
	if c.Stats().Dropped != 0 {
		t.Fatal("outage lost events")
	}
}
