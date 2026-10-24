package usage

import (
	"context"
	"testing"
)

func TestOutputTimingDeliverySnapshotsDoNotSharePointers(t *testing.T) {
	original := Record{OutputTiming: &OutputTiming{Version: 1, WindowTokens: 100, Protocol: "chat", Source: "source_reader", Status: "observed", Coverage: "visible_deltas", TokenQuality: TokenQualityEstimated, FirstBatchBytes: 20, TotalBytes: 200, LargestBatchBytes: 100, MaxInterarrivalNS: 12345}}
	frozen, _, _ := freezeObserverSnapshot(context.Background(), original)
	cloned := cloneRecord(original)
	original.OutputTiming.WindowTokens = 999
	original.OutputTiming.FirstBatchBytes = 999
	original.OutputTiming.TotalBytes = 999
	original.OutputTiming.LargestBatchBytes = 999
	original.OutputTiming.MaxInterarrivalNS = 999
	if frozen.OutputTiming.WindowTokens != 100 || cloned.OutputTiming.WindowTokens != 100 {
		t.Fatal("output evidence retained mutable caller pointer")
	}
	for _, timing := range []*OutputTiming{frozen.OutputTiming, cloned.OutputTiming} {
		if timing.FirstBatchBytes != 20 || timing.TotalBytes != 200 || timing.LargestBatchBytes != 100 || timing.MaxInterarrivalNS != 12345 {
			t.Fatal("batch statistics retained mutable caller pointer")
		}
	}
	cloned.OutputTiming.Status = "unavailable"
	if frozen.OutputTiming.Status != "observed" {
		t.Fatal("observer snapshots share output timing")
	}
}
