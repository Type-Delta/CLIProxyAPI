package collector

import (
	"context"
	"testing"
	"time"
)

func TestMaintenanceDetachSurvivesSuccessfulInflightWrite(t *testing.T) {
	release := make(chan struct{})
	writer := &recordingWriter{block: release}
	current, err := New(writer, testOptions(2, 1))
	if err != nil {
		t.Fatal(err)
	}
	current.Start()
	defer func() { _ = current.Close(context.Background()) }()
	if !current.Enqueue(current.Generation(), validEvent()) {
		t.Fatal("enqueue")
	}
	waitFor(t, func() bool { return writer.calls.Load() == 1 })
	current.Detach()
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := current.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if current.Enqueue(current.Generation(), validEvent()) {
		t.Fatal("in-flight completion reopened maintenance intake")
	}
	if !current.Resume() {
		t.Fatal("maintenance could not resume intake")
	}
}

func TestMaintenanceDrainFlushesPartialBatch(t *testing.T) {
	writer := &recordingWriter{}
	options := testOptions(4, 4)
	options.FlushInterval = time.Hour
	current, err := New(writer, options)
	if err != nil {
		t.Fatal(err)
	}
	current.Start()
	defer func() { _ = current.Close(context.Background()) }()
	if !current.Enqueue(current.Generation(), validEvent()) {
		t.Fatal("enqueue")
	}
	waitFor(t, func() bool { return current.Stats().Depth == 0 })
	current.Detach()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := current.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if writer.count() != 1 {
		t.Fatal("maintenance drain left an unwritten partial batch")
	}
}
