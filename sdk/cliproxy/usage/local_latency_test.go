package usage

import (
	"context"
	"testing"
	"time"
)

func TestObserverSnapshotsOwnTimingValues(t *testing.T) {
	duration := time.Duration(0)
	record := Record{GenerationTime: &duration, FirstTokenLatency: &duration, ProviderLatency: &duration, RoutingTime: &duration}
	frozen, _, _ := freezeObserverSnapshot(context.Background(), record)
	duration = time.Hour
	for _, value := range []*time.Duration{frozen.GenerationTime, frozen.FirstTokenLatency, frozen.ProviderLatency, frozen.RoutingTime} {
		if value == nil || *value != 0 {
			t.Fatal("snapshot retained caller timing pointers")
		}
	}
	clone := cloneRecord(frozen)
	*clone.FirstTokenLatency = time.Second
	*clone.ProviderLatency = time.Second
	*clone.RoutingTime = time.Second
	*clone.GenerationTime = time.Second
	if *frozen.RoutingTime != 0 || *frozen.FirstTokenLatency != 0 || *frozen.ProviderLatency != 0 || *frozen.GenerationTime != 0 {
		t.Fatal("observers share timing pointers")
	}
}
