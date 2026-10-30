package cpauk

import (
	"context"
	"testing"
)

type s1LifecycleBackend struct{ *fakeBackend }

func (*s1LifecycleBackend) UpdateKeyLifecycle(context.Context, []string, []string) error { return nil }

func TestS1LifecycleInvalidatesResponseCache(t *testing.T) {
	s := &service{config: Config{Enabled: true}, backend: &s1LifecycleBackend{&fakeBackend{}}}
	before, err := s.AnalyticsResponseCacheEpoch()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateKeyLifecycle(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	after, err := s.AnalyticsResponseCacheEpoch()
	if err != nil || before == after {
		t.Fatalf("before=%s after=%s err=%v", before, after, err)
	}
}
