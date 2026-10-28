package maintenance

import (
	"context"
	"testing"
)

type abandonStore struct {
	Store
	reason string
}

func (s *abandonStore) AbandonLegacyImport(_ context.Context, reason string) error {
	s.reason = reason
	return nil
}
func TestR2AbandonLegacyImportRequiresReason(t *testing.T) {
	s := &abandonStore{}
	op, ok := StoreOperations(s)["abandon_legacy_import"]
	if !ok {
		t.Fatal("abandon legacy import maintenance is unavailable")
	}
	if _, err := op(context.Background(), map[string]any{}, func(int, string) {}); err == nil {
		t.Fatal("abandon accepted without reason")
	}
	if _, err := op(context.Background(), map[string]any{"reason": "  corrupt source deliberately skipped  "}, func(int, string) {}); err != nil {
		t.Fatal(err)
	}
	if s.reason != "corrupt source deliberately skipped" {
		t.Fatalf("reason=%q", s.reason)
	}
}
