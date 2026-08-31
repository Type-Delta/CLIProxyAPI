package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProviderQuotaSnapshotsAreSanitizedBoundedAndDurable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.db")
	database, err := Open(ctx, Config{Path: path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	reset := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	want := []ProviderQuotaSnapshot{{Provider: "openai", CredentialID: strings.Repeat("a", 64), Model: "gpt-5",
		Available: true, QuotaExceeded: true, NextResetAt: &reset,
		ObservedAt: time.Date(2026, 8, 31, 6, 0, 0, 0, time.UTC)}}
	if err := database.ReplaceProviderQuotaSnapshots(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(ctx); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, Config{Path: path, MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	got, err := database.ProviderQuotaSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Provider != want[0].Provider || got[0].CredentialID != want[0].CredentialID ||
		got[0].Model != want[0].Model || !got[0].Available || !got[0].QuotaExceeded ||
		got[0].NextResetAt == nil || !got[0].NextResetAt.Equal(reset) || got[0].ObservedAt != want[0].ObservedAt {
		t.Fatalf("provider quota snapshots = %+v", got)
	}
	invalid := append([]ProviderQuotaSnapshot(nil), want...)
	invalid[0].CredentialID = "raw-auth-id"
	if err := database.ReplaceProviderQuotaSnapshots(ctx, invalid); err == nil {
		t.Fatal("raw credential identifier was accepted")
	}
	got, err = database.ProviderQuotaSnapshots(ctx)
	if err != nil || len(got) != 1 || got[0].CredentialID != want[0].CredentialID {
		t.Fatalf("invalid replacement changed snapshots: %+v err=%v", got, err)
	}
}
