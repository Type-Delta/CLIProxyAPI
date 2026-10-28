package chstore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProviderQuotaSnapshotsAreSanitizedBoundedAndDurable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.db")
	now := time.Now().UTC()
	database := openDiscoveryStore(t, path, nil, &now)
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
	database = openDiscoveryStore(t, path, nil, &now)
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

func TestC3QuotaStatusSeverityAndReset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	id := strings.Repeat("a", 64)
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	later := now.Add(2 * time.Hour)
	sooner := now.Add(time.Hour)
	snapshots := []ProviderQuotaSnapshot{{Provider: "codex", CredentialID: id, Model: "a", Available: false, NextResetAt: &later, ObservedAt: now}, {Provider: "codex", CredentialID: id, Model: "b", QuotaExceeded: true, NextResetAt: &sooner, ObservedAt: now.Add(time.Minute)}, {Provider: "codex", CredentialID: id, Model: "c", Disabled: true, ObservedAt: now.Add(2 * time.Minute)}}
	if err := db.ReplaceProviderQuotaSnapshots(ctx, snapshots); err != nil {
		t.Fatal(err)
	}
	got, err := db.ProviderCredentials(ctx)
	if err != nil || len(got) != 1 || got[0].Status != "disabled" || got[0].Quota == nil || got[0].Quota.ResetsAt == nil || !got[0].Quota.ResetsAt.Equal(sooner) || !got[0].ObservedAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("credential %+v %v", got, err)
	}
	if err := db.ReplaceProviderQuotaSnapshots(ctx, append(snapshots, snapshots[0])); err == nil {
		t.Fatal("duplicate accepted")
	}
	got, err = db.ProviderCredentials(ctx)
	if err != nil || len(got) != 1 || got[0].Status != "disabled" {
		t.Fatalf("failed replacement changed state %+v %v", got, err)
	}
}
