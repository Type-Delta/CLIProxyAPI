package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/aggregate"
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

func TestAdapterUsesCanonicalClaudeInputForCacheRateAndPricing(t *testing.T) {
	record := validRecord()
	record.Provider = "anthropic"
	record.Detail = coreusage.Detail{
		InputTokens: 30, OutputTokens: 0, CacheReadTokens: 70, TotalTokens: 100,
		TokenBreakdown: coreusage.NewIndependentTokenBreakdown(30, 70, 0, 0, 0, 100),
	}
	source := adaptRecord(record)
	if source.Tokens.Input != 100 {
		t.Fatalf("adapted Claude input = %d, want canonical total 100", source.Tokens.Input)
	}
	if source.Tokens.CacheRead*100 != source.Tokens.Input*70 {
		t.Fatalf("cache rate buckets = input %d, cache read %d; want 70%%", source.Tokens.Input, source.Tokens.CacheRead)
	}

	sanitizer := NewSanitizer(SanitizerOptions{
		NewID: func() (string, error) { return "91a83fb43b38e8770e7648440a89fc48", nil },
	})
	result, err := sanitizer.Sanitize(source)
	if err != nil {
		t.Fatal(err)
	}
	inputRate := model.NanoUSD(1_000_000_000)
	book := aggregate.PriceBook{Rules: []aggregate.PricingRule{{
		ID: "claude-cache-regression", Model: result.Event.Model,
		InputPerMillion: &inputRate, OutputPerMillion: &inputRate,
		CacheReadMultiplier: "0", CacheCreationMultiplier: "1", Source: "test",
	}}}
	priced, err := book.Price(result.Event)
	if err != nil {
		t.Fatal(err)
	}
	if priced.KnownCost == nil || *priced.KnownCost != 30_000 {
		t.Fatalf("Claude cost = %+v, want 30000 nano-USD for 30 uncached tokens", priced.KnownCost)
	}
}

func TestAdapterPreservesOpenAIInputTotalWhenCacheIsIncluded(t *testing.T) {
	record := validRecord()
	record.Provider = "openai"
	record.Detail = coreusage.Detail{
		InputTokens: 100, OutputTokens: 30, CacheReadTokens: 70, TotalTokens: 130,
		TokenBreakdown: coreusage.NewSubsetTokenBreakdown(100, 70, 0, 30, 0, 130),
	}
	source := adaptRecord(record)
	if source.Tokens.Input != 100 || source.Tokens.CacheRead != 70 || source.Tokens.Total != 130 {
		t.Fatalf("adapted OpenAI tokens = %+v, want input=100 cache read=70 total=130", source.Tokens)
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

func TestSanitizerPreservesGenerationNullAndMeasuredZero(t *testing.T) {
	sanitizer := NewSanitizer(SanitizerOptions{})
	record := validRecord()
	legacy, err := sanitizer.Sanitize(adaptRecord(record))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Event.GenerationTimeMS != nil {
		t.Fatal("historical record fabricated generation")
	}
	for _, duration := range []time.Duration{0, 42 * time.Millisecond} {
		record.GenerationTime = &duration
		result, err := sanitizer.Sanitize(adaptRecord(record))
		if err != nil {
			t.Fatal(err)
		}
		if result.Event.GenerationTimeMS == nil || *result.Event.GenerationTimeMS != duration.Milliseconds() {
			t.Fatalf("generation=%v", result.Event.GenerationTimeMS)
		}
	}
	negative := -time.Millisecond
	record.GenerationTime = &negative
	if _, err := sanitizer.Sanitize(adaptRecord(record)); err == nil {
		t.Fatal("accepted negative generation time")
	}
}
