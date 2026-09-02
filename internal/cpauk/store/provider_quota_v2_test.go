package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

func TestProviderCredentialsAggregatesSanitizedUsageErrorsAndDurableStatus(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	start := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	credentialID := strings.Repeat("c", 64)
	rawAuthID := "raw-auth-secret-must-not-escape"
	authType := "oauth"
	firstError, latestError := "upstream", "rate_limit"
	events := []model.Event{
		v2Event("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "11111111111111111111111111111111", strings.Repeat("d", 64), start.Add(time.Minute), false, &firstError, 0, 0, 10, 20),
		v2Event("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "22222222222222222222222222222222", strings.Repeat("d", 64), start.Add(2*time.Minute), false, &latestError, 0, 0, 10, 20),
		v2Event("cccccccccccccccccccccccccccccccc", "33333333333333333333333333333333", strings.Repeat("d", 64), start.Add(3*time.Minute), true, nil, 0, 0, 10, 20),
	}
	for index := range events {
		events[index].CredentialID = &credentialID
		algorithm := model.CredentialIDAlgorithm
		events[index].CredentialIDAlgorithm = &algorithm
		events[index].AuthType = &authType
	}
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	reset := start.Add(time.Hour)
	observed := start.Add(4 * time.Minute)
	if err := database.ReplaceProviderQuotaSnapshots(ctx, []ProviderQuotaSnapshot{{
		Provider: "provider-v2", CredentialID: credentialID, Available: false, QuotaExceeded: true,
		NextResetAt: &reset, ObservedAt: observed,
	}}); err != nil {
		t.Fatal(err)
	}

	credentials, err := database.ProviderCredentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(credentials) != 1 {
		t.Fatalf("provider credentials = %+v", credentials)
	}
	credential := credentials[0]
	if credential.CredentialID != credentialID || credential.Provider != "provider-v2" || credential.AuthType != authType || credential.Requests != 3 || credential.Failed != 2 {
		t.Fatalf("provider credential totals = %+v", credential)
	}
	if credential.Status != "quota_exceeded" || credential.LastErrorClass == nil || *credential.LastErrorClass != latestError || credential.LastErrorAt == nil || !credential.LastErrorAt.Equal(events[1].RequestedAt) {
		t.Fatalf("provider credential state = %+v", credential)
	}
	if credential.Quota == nil || credential.Quota.ResetsAt == nil || !credential.Quota.ResetsAt.Equal(reset) || !credential.ObservedAt.Equal(observed) {
		t.Fatalf("provider credential quota = %+v", credential)
	}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), rawAuthID) || strings.Contains(string(encoded), `"auth_index"`) {
		t.Fatalf("provider credentials exposed raw identity: %s", encoded)
	}
}

func TestProviderCredentialsCountsDistinctRequestsAcrossRawRetainedAndAuthGroups(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "analytics.db"), MaxStorageBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(context.Background()) })
	start := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	credentialID := strings.Repeat("c", 64)
	requestID := strings.Repeat("1", 32)
	authTypes := []string{"api-key", "oauth"}
	events := []model.Event{
		v2Event(strings.Repeat("a", 32), requestID, strings.Repeat("d", 64), start.Add(time.Minute), false, pointerString("upstream"), 0, 0, 10, 20),
		v2Event(strings.Repeat("b", 32), requestID, strings.Repeat("d", 64), start.Add(time.Hour+time.Minute), true, nil, 0, 0, 10, 20),
	}
	for index := range events {
		events[index].CredentialID = &credentialID
		algorithm := model.CredentialIDAlgorithm
		events[index].CredentialIDAlgorithm = &algorithm
		events[index].AuthType = &authTypes[index]
	}
	if err := database.WriteBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ApplyRetention(ctx, start.Add(time.Hour), 100); err != nil {
		t.Fatal(err)
	}

	credentials, err := database.ProviderCredentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(credentials) != 1 || credentials[0].Requests != 1 || credentials[0].Failed != 1 || credentials[0].AuthType != "oauth" {
		t.Fatalf("provider credentials = %+v, want one distinct request and latest auth type", credentials)
	}
}

func pointerString(value string) *string { return &value }
