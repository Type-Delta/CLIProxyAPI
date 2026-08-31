package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestSanitizerCopiesOnlyAllowlistedFields(t *testing.T) {
	ids := []string{"91a83fb43b38e8770e7648440a89fc48"}
	sanitizer := NewSanitizer(SanitizerOptions{
		IdentityKey:     [32]byte{1, 2, 3},
		StoreCredential: true,
		NewID: func() (string, error) {
			value := ids[0]
			ids = ids[1:]
			return value, nil
		},
	})
	record := validRecord()
	record.Source = "person@example.invalid"
	record.Fail.Body = "secret failure body"
	record.ResponseHeaders = http.Header{
		"Authorization": []string{"Bearer secret-token"},
		"User-Agent":    []string{"private-agent"},
	}
	source := adaptRecord(record)
	source.EndpointClass = "POST /v1/responses"
	result, err := sanitizer.Sanitize(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Event.Validate(); err != nil {
		t.Fatalf("sanitized event does not satisfy Event v1: %v", err)
	}
	if result.Event.KeyID != model.KeyID(record.APIKey) || result.Event.EndpointClass != "responses" {
		t.Fatalf("sanitized identity or endpoint = %#v", result.Event)
	}
	if result.Event.CredentialID == nil || result.Event.CredentialIDAlgorithm == nil {
		t.Fatal("credential pseudonym missing")
	}
	encoded, err := json.Marshal(result.Event)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{record.APIKey, record.AuthID, record.AuthIndex, record.Source, record.Fail.Body, "secret-token", "private-agent"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("sanitized event contains forbidden source value %q", forbidden)
		}
	}
}

func TestSanitizerNeverPersistsLowEntropyRawKey(t *testing.T) {
	const rawKey = "weak-key"
	sanitizer := NewSanitizer(SanitizerOptions{
		NewID: func() (string, error) { return "91a83fb43b38e8770e7648440a89fc48", nil },
	})
	record := validRecord()
	record.APIKey = rawKey
	result, err := sanitizer.Sanitize(adaptRecord(record))
	if err != nil {
		t.Fatal(err)
	}
	if result.Event.KeyID != model.KeyID(rawKey) || result.Event.KeyID == rawKey {
		t.Fatalf("low-entropy key identity=%q", result.Event.KeyID)
	}
	encoded, err := json.Marshal(result.Event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), rawKey) {
		t.Fatalf("sanitized event contains raw low-entropy key: %s", encoded)
	}
}

func TestSanitizerPreservesSyntheticQualityAndTruncatesByBytes(t *testing.T) {
	sanitizer := NewSanitizer(SanitizerOptions{
		NewID: func() (string, error) { return "91a83fb43b38e8770e7648440a89fc48", nil },
	})
	record := validRecord()
	record.RequestIDQuality = coreusage.RequestIDSynthetic
	record.Provider = strings.Repeat("界", 100)
	result, err := sanitizer.Sanitize(adaptRecord(record))
	if err != nil {
		t.Fatal(err)
	}
	if result.Event.RequestIDQuality != model.RequestIDSynthetic {
		t.Fatalf("request quality = %q", result.Event.RequestIDQuality)
	}
	if len(result.Event.Provider) > model.MaxStoredStringBytes || !strings.HasPrefix(result.Event.Provider, "界") {
		t.Fatalf("provider was not truncated on a UTF-8 boundary: %q", result.Event.Provider)
	}
	if result.TruncatedFields != 1 {
		t.Fatalf("truncated fields = %d, want 1", result.TruncatedFields)
	}
}

func TestSanitizerPreservesOrInfersTokenQuality(t *testing.T) {
	sanitizer := NewSanitizer(SanitizerOptions{
		NewID: func() (string, error) { return "91a83fb43b38e8770e7648440a89fc48", nil },
	})

	record := validRecord()
	record.Detail.TokenQuality = coreusage.TokenQualityEstimated
	result, err := sanitizer.Sanitize(adaptRecord(record))
	if err != nil {
		t.Fatal(err)
	}
	if result.Event.Tokens.Quality != model.TokenQualityEstimated {
		t.Fatalf("explicit quality = %q", result.Event.Tokens.Quality)
	}

	record = validRecord()
	record.Detail = coreusage.Detail{}
	result, err = sanitizer.Sanitize(adaptRecord(record))
	if err != nil {
		t.Fatal(err)
	}
	if result.Event.Tokens.Quality != model.TokenQualityMissing {
		t.Fatalf("zero-token inferred quality = %q", result.Event.Tokens.Quality)
	}
}

func TestSanitizerRejectsInvalidSourceValues(t *testing.T) {
	sanitizer := NewSanitizer(SanitizerOptions{})
	tests := []struct {
		name   string
		mutate func(*coreusage.Record)
	}{
		{name: "missing key", mutate: func(record *coreusage.Record) { record.APIKey = " " }},
		{name: "zero timestamp", mutate: func(record *coreusage.Record) { record.RequestedAt = time.Time{} }},
		{name: "negative latency", mutate: func(record *coreusage.Record) { record.Latency = -time.Millisecond }},
		{name: "negative token", mutate: func(record *coreusage.Record) { record.Detail.InputTokens = -1 }},
		{name: "bad status", mutate: func(record *coreusage.Record) { record.Fail.StatusCode = 99 }},
		{name: "bad utf8", mutate: func(record *coreusage.Record) { record.Model = string([]byte{0xff}) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := validRecord()
			test.mutate(&record)
			if _, err := sanitizer.Sanitize(adaptRecord(record)); err == nil {
				t.Fatal("invalid record was accepted")
			}
		})
	}
}

func TestAdapterContainsPanicsAndCountsRejects(t *testing.T) {
	intake := &fakeIntake{generation: 7, panicOnEnqueue: true}
	adapter := NewAdapter(intake, NewSanitizer(SanitizerOptions{
		NewID: func() (string, error) { return "91a83fb43b38e8770e7648440a89fc48", nil },
	}))
	ctx := context.Background()
	adapter.HandleUsage(ctx, validRecord())
	if adapter.Dropped() != 1 || intake.rejected != 1 {
		t.Fatalf("panic counts = dropped %d rejected %d", adapter.Dropped(), intake.rejected)
	}

	record := validRecord()
	record.APIKey = ""
	intake.panicOnEnqueue = false
	adapter.HandleUsage(ctx, record)
	if adapter.Dropped() != 2 || intake.rejected != 2 {
		t.Fatalf("reject counts = dropped %d rejected %d", adapter.Dropped(), intake.rejected)
	}
}

func validRecord() coreusage.Record {
	return coreusage.Record{
		ProxyRequestID: "d1371f43e6b8362d05d7567ed5fcc2ad",
		EndpointClass:  "messages",
		Provider:       " Provider-A17C92 ",
		ExecutorType:   "executor-47c8",
		Model:          "model-f93b",
		Alias:          "alias-53b1",
		APIKey:         "fixture-secret-f10d6a89",
		AuthID:         "person@example.invalid.json",
		AuthIndex:      "Index-Case-91",
		AuthType:       "oauth",
		ServiceTier:    "standard",
		RequestedAt:    time.Date(2026, 8, 31, 4, 5, 6, 0, time.UTC),
		Latency:        120 * time.Millisecond,
		TTFT:           23 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens: 70, OutputTokens: 30, ReasoningTokens: 10,
			CachedTokens: 10, CacheReadTokens: 5, CacheCreationTokens: 5,
			TotalTokens: 100,
		},
	}
}

type fakeIntake struct {
	generation     uint64
	rejected       int64
	truncated      int64
	panicOnEnqueue bool
}

func (f *fakeIntake) Generation() uint64 { return f.generation }
func (f *fakeIntake) Enqueue(generation uint64, _ Event) bool {
	if f.panicOnEnqueue {
		panic("injected enqueue panic")
	}
	return generation == f.generation
}
func (f *fakeIntake) Rejected()         { f.rejected++ }
func (f *fakeIntake) Truncated(n int64) { f.truncated += n }
