package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

type memoryDestination struct {
	events      map[string]model.Event
	checkpoints map[string][]byte
	failWrite   bool
}

func (d *memoryDestination) WriteImportBatch(_ context.Context, events []model.Event, _ string) (int64, error) {
	if d.failWrite {
		return 0, errors.New("injected write failure")
	}
	copyEvents := make(map[string]model.Event, len(d.events)+len(events))
	for key, value := range d.events {
		copyEvents[key] = value
	}
	var inserted int64
	for _, event := range events {
		if _, exists := copyEvents[event.AttemptID]; !exists {
			copyEvents[event.AttemptID] = event
			inserted++
		}
	}
	d.events = copyEvents
	return inserted, nil
}

func (d *memoryDestination) LoadImportCheckpoint(_ context.Context, batchID string) ([]byte, bool, error) {
	value, ok := d.checkpoints[batchID]
	return append([]byte(nil), value...), ok, nil
}

func (d *memoryDestination) SaveImportCheckpoint(_ context.Context, batchID, _, _ string, _ int64, _ int, checkpoint []byte, _ bool, _ [5]int64) error {
	d.checkpoints[batchID] = append([]byte(nil), checkpoint...)
	return nil
}

func TestDryRunCommitResumeAndChunkRollback(t *testing.T) {
	destination := &memoryDestination{events: map[string]model.Event{}, checkpoints: map[string][]byte{}}
	worker := Importer{Destination: destination, Transform: fixtureTransform}
	source := &SliceSource{SourceKind: "cpauk-v1.15.0", ID: "fixture", Rows: []SourceRow{{Value: 0}, {Value: 1}, {Value: 2}}}
	dryRun, err := worker.Run(context.Background(), source, Options{BatchID: "batch-test", DryRun: true, ChunkSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if dryRun.RowsRead != 3 || dryRun.Transformed != 2 || dryRun.Skipped != 1 || dryRun.Inserted != 0 || !dryRun.Reconciled || len(destination.events) != 0 {
		t.Fatalf("dry run=%+v events=%d", dryRun, len(destination.events))
	}
	source = &SliceSource{SourceKind: "cpauk-v1.15.0", ID: "fixture", Rows: []SourceRow{{Value: 0}, {Value: 1}, {Value: 2}}}
	committed, err := worker.Run(context.Background(), source, Options{BatchID: "batch-test", ChunkSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if committed.Inserted != 2 || !committed.Reconciled || len(destination.events) != 2 {
		t.Fatalf("commit=%+v events=%d", committed, len(destination.events))
	}
	source = &SliceSource{SourceKind: "cpauk-v1.15.0", ID: "fixture", Rows: []SourceRow{{Value: 0}, {Value: 1}, {Value: 2}}}
	resumed, err := worker.Run(context.Background(), source, Options{BatchID: "batch-test", ChunkSize: 2, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.RowsRead != committed.RowsRead || len(destination.events) != 2 {
		t.Fatalf("resume=%+v", resumed)
	}

	failing := &memoryDestination{events: map[string]model.Event{}, checkpoints: map[string][]byte{}, failWrite: true}
	source = &SliceSource{SourceKind: "cpauk-v1.15.0", ID: "fixture", Rows: []SourceRow{{Value: 0}, {Value: 2}}}
	_, err = (&Importer{Destination: failing, Transform: fixtureTransform}).Run(context.Background(), source, Options{BatchID: "batch-fail", ChunkSize: 2})
	if err == nil || len(failing.events) != 0 || len(failing.checkpoints) != 0 {
		t.Fatalf("failure err=%v events=%d checkpoints=%d", err, len(failing.events), len(failing.checkpoints))
	}
}

func TestCheckpointRejectsTampering(t *testing.T) {
	state := checkpoint{Version: 1, BatchID: "batch-test", SourceKind: "kind", Fingerprint: "fingerprint"}
	data, err := state.encode()
	if err != nil {
		t.Fatal(err)
	}
	var decoded checkpoint
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded.Offset++
	if err := decoded.validate("batch-test", "kind", "fingerprint"); err == nil {
		t.Fatal("tampered checkpoint passed")
	}
}

func fixtureTransform(_ context.Context, row SourceRow) (model.Event, bool, error) {
	value, ok := row.Value.(int)
	if !ok {
		return model.Event{}, false, fmt.Errorf("unexpected source value")
	}
	if value == 1 {
		return model.Event{}, true, nil
	}
	id := fmt.Sprintf("%032x", value+1)
	return model.Event{
		SchemaVersion: 1, AttemptID: id, ProxyRequestID: id, RequestIDQuality: model.RequestIDObserved,
		KeyID: fmt.Sprintf("%064x", value+1), RequestedAt: time.Unix(int64(value+1), 0).UTC(), Provider: "p",
		ExecutorType: "e", Model: "m", EndpointClass: "responses", Succeeded: true,
		Tokens: model.TokenUsage{Total: 1, Schema: "normalized-v1", Quality: model.TokenQualityExact},
	}, false, nil
}
