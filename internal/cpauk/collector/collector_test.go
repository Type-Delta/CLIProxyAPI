package collector

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func TestCollectorBatchesAndDrainsOnClose(t *testing.T) {
	writer := &recordingWriter{}
	current, err := New(writer, testOptions(4, 2))
	if err != nil {
		t.Fatal(err)
	}
	current.Start()
	generation := current.Generation()
	for range 3 {
		if !current.Enqueue(generation, validEvent()) {
			t.Fatal("enqueue failed")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := current.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := writer.count(); got != 3 {
		t.Fatalf("written events = %d, want 3", got)
	}
	if current.Enqueue(generation, validEvent()) {
		t.Fatal("enqueue succeeded after close")
	}
}

func TestCollectorSaturationNeverBlocksAndDropsNewest(t *testing.T) {
	block := make(chan struct{})
	writer := &recordingWriter{block: block}
	current, err := New(writer, testOptions(1, 1))
	if err != nil {
		t.Fatal(err)
	}
	current.Start()
	generation := current.Generation()
	if !current.Enqueue(generation, validEvent()) {
		t.Fatal("first enqueue failed")
	}
	waitFor(t, func() bool { return writer.calls.Load() == 1 })
	if !current.Enqueue(generation, validEvent()) {
		t.Fatal("queue fill failed")
	}
	start := time.Now()
	if current.Enqueue(generation, validEvent()) {
		t.Fatal("saturated queue accepted newest event")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Fatalf("saturated enqueue waited %s", elapsed)
	}
	close(block)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := current.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCollectorGenerationRejectsStaleAdapter(t *testing.T) {
	current, err := New(&recordingWriter{}, testOptions(2, 1))
	if err != nil {
		t.Fatal(err)
	}
	current.Start()
	stale := current.Generation()
	current.Detach()
	if current.Enqueue(stale, validEvent()) {
		t.Fatal("detached generation accepted an event")
	}
	if current.Retry() {
		t.Fatal("detached collector without a circuit accepted retry")
	}
	if current.Enqueue(stale, validEvent()) {
		t.Fatal("stale generation crossed a detach boundary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := current.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCollectorWorkerPanicRestartsThenOpensCircuit(t *testing.T) {
	writer := &recordingWriter{panicAlways: true}
	var state atomic.Value
	options := testOptions(16, 1)
	options.RestartDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	options.Callbacks.State = func(value model.AnalyticsState, _ string) { state.Store(value) }
	current, err := New(writer, options)
	if err != nil {
		t.Fatal(err)
	}
	current.Start()
	for index := 0; index < 4; index++ {
		waitFor(t, func() bool { return current.accepting.Load() })
		if !current.Enqueue(current.Generation(), validEvent()) {
			t.Fatalf("enqueue %d failed", index)
		}
		waitFor(t, func() bool { return writer.calls.Load() >= int64(index+1) })
	}
	waitFor(t, func() bool {
		value := state.Load()
		return value != nil && value.(model.AnalyticsState) == model.StateCircuitOpen
	})
	if current.accepting.Load() {
		t.Fatal("panic loop left intake attached")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := current.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCollectorPermanentFailureWaitsForExplicitRetry(t *testing.T) {
	writer := &recordingWriter{errs: []error{permanentTestError{}, nil}}
	options := testOptions(4, 1)
	options.FailureThreshold = 1
	current, err := New(writer, options)
	if err != nil {
		t.Fatal(err)
	}
	current.Start()
	if !current.Enqueue(current.Generation(), validEvent()) {
		t.Fatal("enqueue failed")
	}
	waitFor(t, func() bool { return writer.calls.Load() == 1 && !current.accepting.Load() && current.retryable.Load() })
	time.Sleep(5 * time.Millisecond)
	if writer.calls.Load() != 1 {
		t.Fatal("permanent failure retried without an administrator action")
	}
	if !current.Retry() {
		t.Fatal("explicit retry rejected")
	}
	waitFor(t, func() bool { return writer.calls.Load() == 2 && current.accepting.Load() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := current.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCollectorCloseHonorsDeadlineWithStalledWriter(t *testing.T) {
	writer := &recordingWriter{waitForContext: true}
	var abandoned atomic.Int64
	options := testOptions(2, 1)
	options.Callbacks.Abandoned = func(count int64) { abandoned.Store(count) }
	current, err := New(writer, options)
	if err != nil {
		t.Fatal(err)
	}
	current.Start()
	if !current.Enqueue(current.Generation(), validEvent()) {
		t.Fatal("enqueue failed")
	}
	waitFor(t, func() bool { return writer.calls.Load() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = current.Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Close exceeded its bound: %s", elapsed)
	}
	if abandoned.Load() < 1 {
		t.Fatalf("abandoned events = %d, want at least 1", abandoned.Load())
	}
}

type recordingWriter struct {
	mu             sync.Mutex
	events         []model.Event
	errs           []error
	block          <-chan struct{}
	waitForContext bool
	panicAlways    bool
	calls          atomic.Int64
}

func (w *recordingWriter) WriteBatch(ctx context.Context, events []model.Event) error {
	w.calls.Add(1)
	if w.panicAlways {
		panic("injected writer panic")
	}
	if w.waitForContext {
		<-ctx.Done()
		return ctx.Err()
	}
	if w.block != nil {
		<-w.block
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.errs) > 0 {
		err := w.errs[0]
		w.errs = w.errs[1:]
		if err != nil {
			return err
		}
	}
	w.events = append(w.events, events...)
	return nil
}

func (w *recordingWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.events)
}

type permanentTestError struct{}

func (permanentTestError) Error() string    { return "permanent" }
func (permanentTestError) Permanent() bool  { return true }
func (permanentTestError) Category() string { return "corrupt" }

func testOptions(capacity, batch int) Options {
	return Options{
		Capacity: capacity, BatchSize: batch, FlushInterval: time.Hour,
		FailureThreshold: 5,
	}
}

func validEvent() model.Event {
	return model.Event{
		SchemaVersion: model.EventSchemaVersion,
		AttemptID:     "91a83fb43b38e8770e7648440a89fc48", ProxyRequestID: "d1371f43e6b8362d05d7567ed5fcc2ad",
		RequestIDQuality: model.RequestIDObserved,
		KeyID:            model.KeyID("fixture-secret-f10d6a89"), RequestedAt: time.Now().UTC(),
		Provider: "provider", ExecutorType: "executor", Model: "model", EndpointClass: "responses",
		Succeeded: true, Tokens: model.TokenUsage{Schema: "normalized-v1", Quality: model.TokenQualityExact},
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}
