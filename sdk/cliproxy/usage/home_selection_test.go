package usage

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHomeSelectionContextAndPublishSnapshot(t *testing.T) {
	ctx := WithHomeSelection(t.Context(), HomeSelection{Model: "route"})
	followup := HomeSelectionFromContext(ctx)
	followup.Reused = true
	if HomeSelectionFromContext(ctx).Reused {
		t.Fatal("follow-up mutated the first turn")
	}
	mgr := NewManager(10)
	t.Cleanup(mgr.Stop)
	plugin := &captureUsagePlugin{records: make(chan Record, 2)}
	mgr.Register(plugin)
	mgr.Publish(ctx, Record{})
	mgr.Publish(ctx, Record{HomeSelection: followup})
	first, second := <-plugin.records, <-plugin.records
	if first.HomeSelection == nil || first.HomeSelection.Reused || second.HomeSelection == nil || !second.HomeSelection.Reused {
		t.Fatalf("lost attempt metadata: first=%+v second=%+v", first.HomeSelection, second.HomeSelection)
	}
}

func TestHomeSelectionCloneAndObserverBudget(t *testing.T) {
	original := Record{HomeSelection: &HomeSelection{Reused: true, Model: strings.Repeat("界", MaxObserverSnapshotBytes), CredentialPolicy: strings.Repeat("p", MaxObserverSnapshotBytes)}}
	cloned := cloneRecord(original)
	cloned.HomeSelection.Model = "changed"
	if original.HomeSelection.Model == "changed" {
		t.Fatal("clone shares HomeSelection")
	}
	frozen, _, size := freezeObserverSnapshot(t.Context(), original)
	if frozen.HomeSelection == original.HomeSelection {
		t.Fatal("snapshot shares HomeSelection")
	}
	if size > int64(MaxObserverSnapshotBytes) || len(frozen.HomeSelection.Model)+len(frozen.HomeSelection.CredentialPolicy) > MaxObserverSnapshotBytes {
		t.Fatalf("unbounded observer snapshot: %d", size)
	}
	if !utf8.ValidString(frozen.HomeSelection.Model) || !utf8.ValidString(frozen.HomeSelection.CredentialPolicy) {
		t.Fatal("snapshot split UTF-8")
	}
	if len(original.HomeSelection.Model) != 3*MaxObserverSnapshotBytes {
		t.Fatal("observer mutated trusted record")
	}
}
