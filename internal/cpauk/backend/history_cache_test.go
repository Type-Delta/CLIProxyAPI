package backend

import (
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestHistoryCacheBoundsAndLRU(t *testing.T) {
	var cache HistoryResultCache
	value := HistoryCacheValue{Operation: model.OperationSummary}
	for i := 0; i < HistoryCacheMaxEntries; i++ {
		cache.Put(fmt.Sprint(i), value)
	}
	if _, ok := cache.Get("0"); !ok {
		t.Fatal("missing oldest entry")
	}
	cache.Put("next", value)
	if _, ok := cache.Get("1"); ok {
		t.Fatal("least recently used entry survived")
	}
	if _, ok := cache.Get("0"); !ok {
		t.Fatal("recently read entry evicted")
	}
	entries, size := cache.Stats()
	if entries != HistoryCacheMaxEntries || size > HistoryCacheMaxBytes {
		t.Fatalf("bounds: %d %d", entries, size)
	}
	cache.Clear()
	// A key counts toward the same byte budget as the encoded result.
	large := strings.Repeat("x", HistoryCacheMaxBytes/2)
	cache.Put(large+"1", value)
	cache.Put(large+"2", value)
	if entries, size := cache.Stats(); entries != 1 || size > HistoryCacheMaxBytes {
		t.Fatalf("byte eviction: %d %d", entries, size)
	}
	cache.Put(strings.Repeat("z", HistoryCacheMaxBytes+1), value)
	if entries, _ := cache.Stats(); entries != 1 {
		t.Fatal("oversized entry admitted")
	}
	cache.Clear()
	if entries, size := cache.Stats(); entries != 0 || size != 0 {
		t.Fatal("clear retained entries")
	}
}
