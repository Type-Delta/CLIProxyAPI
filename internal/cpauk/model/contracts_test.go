package model

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	keyIDA = "19fcf81df9578ec9d87eb62b6dffdd5ab6a7be6e373cc725ae33c1f7884e97e7"
	keyIDB = "347a11cc66379718b57489d5bc951fdc3d47f150449c028cb5dff4f27fac23de"
)

func TestKeyIDTrimsAndHashes(t *testing.T) {
	want := KeyID("fixture-secret-f10d6a89")
	if got := KeyID(" \tfixture-secret-f10d6a89\n"); got != want {
		t.Fatalf("KeyID() = %q, want %q", got, want)
	}
	if !IsFullKeyID(want) {
		t.Fatalf("KeyID() produced invalid digest %q", want)
	}
}

func TestShortKeyIDsLengthenCollisionsAndCollapseDuplicates(t *testing.T) {
	first := "aaaaaaaaaaaaaa11" + strings.Repeat("1", 48)
	second := "aaaaaaaaaaaaaa22" + strings.Repeat("2", 48)
	third := "bbbbbbbbbbbb" + strings.Repeat("3", 52)
	got, err := ShortKeyIDs([]string{first, second, third, first})
	if err != nil {
		t.Fatal(err)
	}
	if got[first] != first[:16] || got[second] != second[:16] {
		t.Fatalf("colliding IDs were not lengthened by two characters: %#v", got)
	}
	if got[third] != third[:ShortKeyIDMinLength] {
		t.Fatalf("non-colliding ID = %q", got[third])
	}
	if len(got) != 3 {
		t.Fatalf("duplicate full IDs were not collapsed: %#v", got)
	}
}

func TestCredentialIDPrecedenceNormalizationAndNull(t *testing.T) {
	identityKey := []byte("identity-fixture-key-32-byte-2f7")
	fromIndex, err := CredentialID(identityKey, " Provider-A17C92 ", " Index-Case-91 ", "fallback-4d2")
	if err != nil {
		t.Fatal(err)
	}
	trimmed, err := CredentialID(identityKey, "provider-a17c92", "Index-Case-91", "different-fallback")
	if err != nil {
		t.Fatal(err)
	}
	if fromIndex == nil || trimmed == nil || *fromIndex != *trimmed {
		t.Fatalf("AuthIndex precedence or trim/provider normalization changed: %v %v", fromIndex, trimmed)
	}
	differentCase, err := CredentialID(identityKey, "provider-a17c92", "index-case-91", "")
	if err != nil {
		t.Fatal(err)
	}
	if differentCase == nil || *differentCase == *fromIndex {
		t.Fatal("case-sensitive source identity was folded")
	}
	fallback, err := CredentialID(identityKey, "provider-a17c92", "", " auth-ref-4d2 ")
	if err != nil || fallback == nil {
		t.Fatalf("AuthID fallback = %v, %v", fallback, err)
	}
	missing, err := CredentialID(identityKey, "provider-a17c92", " ", "")
	if err != nil || missing != nil {
		t.Fatalf("missing identity = %v, %v", missing, err)
	}
}

func TestNanoUSDExactJSONAndPricingRounding(t *testing.T) {
	for input, want := range map[string]string{
		"10.035": "10.035", "15.0525": "15.0525", "0.000000001": "0.000000001",
		"-0.5": "-0.5", "0": "0",
	} {
		value, err := ParseNanoUSD(input)
		if err != nil {
			t.Fatalf("ParseNanoUSD(%q): %v", input, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != `"`+want+`"` {
			t.Fatalf("MarshalJSON(%q) = %s", input, encoded)
		}
	}
	price, _ := ParseNanoUSD("15.0525")
	cost, err := CostForTokens(1, price)
	if err != nil {
		t.Fatal(err)
	}
	if cost != 15_053 {
		t.Fatalf("half-away rounding = %d nano-USD, want 15053", cost)
	}
	negativeCost, err := CostForTokens(1, -price)
	if err != nil {
		t.Fatal(err)
	}
	if negativeCost != -15_053 {
		t.Fatalf("negative half-away rounding = %d nano-USD, want -15053", negativeCost)
	}
}

func TestEventV1Validation(t *testing.T) {
	event := validEvent()
	if err := event.Validate(); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
	event.LatencyMS = -1
	if err := event.Validate(); err == nil {
		t.Fatal("negative latency accepted")
	}
	event = validEvent()
	event.Provider = " " + event.Provider
	if err := event.Validate(); err == nil {
		t.Fatal("untrimmed provider accepted")
	}
}

func TestParseQueryStrictBoundsAndFilters(t *testing.T) {
	valid := `{"schema_version":1,"operation":"leaderboard","start":"2026-08-01T00:00:00Z","end":"2026-08-08T00:00:00Z","time_zone":"Asia/Bangkok","key_ids":["` + keyIDA + `","` + keyIDA + `"],"filters":{"provider":["provider-a17c92"]},"page_size":25,"sort_by":"tokens"}`
	query, err := ParseQuery([]byte(valid))
	if err != nil {
		t.Fatalf("valid query rejected: %v", err)
	}
	if len(query.KeyIDs) != 1 {
		t.Fatalf("key IDs were not deduplicated: %#v", query.KeyIDs)
	}

	tests := []string{
		strings.Replace(valid, `"sort_by":"tokens"`, `"sort_by":"score"`, 1),
		strings.Replace(valid, `"provider"`, `"unknown_filter"`, 1),
		strings.Replace(valid, `"page_size":25`, `"page_size":501`, 1),
		strings.Replace(valid, `"time_zone":"Asia/Bangkok"`, `"time_zone":"GMT+7"`, 1),
		strings.Replace(valid, `"sort_by":"tokens"`, `"sort_by":"tokens","unknown":true`, 1),
	}
	for _, input := range tests {
		if _, err := ParseQuery([]byte(input)); err == nil {
			t.Fatalf("invalid query accepted: %s", input)
		}
	}
}

func TestQueryRejectsExcessiveRangeAndBuckets(t *testing.T) {
	query := Query{
		SchemaVersion: 1,
		Operation:     OperationTimeseries,
		Start:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:           time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		TimeZone:      "UTC",
		BucketWidth:   "1m",
	}
	if err := query.Validate(); err == nil {
		t.Fatal("query exceeding bucket limit was accepted")
	}
	query.BucketWidth = "1d"
	query.End = query.Start.Add((MaxQueryRangeDays + 1) * 24 * time.Hour)
	if err := query.Validate(); err == nil {
		t.Fatal("query exceeding range limit was accepted")
	}
}

func TestCursorRoundTripAndBinding(t *testing.T) {
	codec := testCursorCodec(t)
	cursorValue := Cursor{Version: 1, Operation: OperationLeaderboard, SortBy: LeaderboardSortCost, Selection: keyIDB, Metric: "10.035", KeyID: keyIDA, Rank: 2}
	encoded, err := codec.Encode(cursorValue)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := codec.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != cursorValue {
		t.Fatalf("cursor round trip = %#v, want %#v", decoded, cursorValue)
	}
	envelope, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(envelope), keyIDA) || strings.Contains(string(envelope), keyIDB) {
		t.Fatalf("opaque cursor reveals a key digest: %q", envelope)
	}
}

func TestDimensionsCursorBindsFullNormalizedSelection(t *testing.T) {
	query := Query{
		SchemaVersion: QuerySchemaVersion,
		Operation:     OperationDimensions,
		Start:         time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		End:           time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		TimeZone:      "Asia/Bangkok",
		KeyIDs:        []string{keyIDB, keyIDA},
		Filters: map[string]json.RawMessage{
			"provider": json.RawMessage(`["provider-5d90e8","provider-a17c92"]`),
		},
		PageSize:  25,
		Dimension: "model",
	}
	if err := query.Validate(); err != nil {
		t.Fatalf("validate first dimensions page: %v", err)
	}
	selection, err := query.SelectionDigest()
	if err != nil {
		t.Fatal(err)
	}
	codec := testCursorCodec(t)
	cursor, err := codec.Encode(Cursor{
		Version: 1, Operation: OperationDimensions, Selection: selection,
		Metric: "200", Value: "model-f93b", Rank: 25,
	})
	if err != nil {
		t.Fatal(err)
	}
	query.Cursor = cursor
	if err := query.ValidateCursor(codec, CursorTransportBody); err != nil {
		t.Fatalf("validate dimensions page 2: %v", err)
	}

	query.End = query.End.Add(-time.Hour)
	if err := query.ValidateCursor(codec, CursorTransportBody); err == nil || !strings.Contains(err.Error(), "normalized query selection") {
		t.Fatalf("changed selection returned %v, want cursor mismatch", err)
	}
}

func TestIdentityCursorsAreForbiddenInGET(t *testing.T) {
	query := Query{Operation: OperationLeaderboard, KeyIDs: []string{keyIDA}}
	if query.CursorAllowedInGET() {
		t.Fatal("leaderboard key cursor was allowed in GET")
	}
	query = Query{Operation: OperationDimensions, Dimension: "credential"}
	if query.CursorAllowedInGET() {
		t.Fatal("credential dimension cursor was allowed in GET")
	}
}

func TestCursorRejectsZeroTimeAndNegativeMetrics(t *testing.T) {
	zero := time.Time{}
	cases := []Cursor{
		{Version: 1, Operation: OperationEvents, Selection: keyIDA, RequestedAt: &zero, AttemptID: "91a83fb43b38e8770e7648440a89fc48"},
		{Version: 1, Operation: OperationDimensions, Selection: keyIDA, Metric: "-1", Value: "model-f93b", Rank: 1},
		{Version: 1, Operation: OperationLeaderboard, SortBy: LeaderboardSortTokens, Selection: keyIDA, Metric: "-1", KeyID: keyIDB, Rank: 1},
		{Version: 1, Operation: OperationLeaderboard, SortBy: LeaderboardSortCost, Selection: keyIDA, Metric: "-0.1", KeyID: keyIDB, Rank: 1},
	}
	for _, cursor := range cases {
		if err := cursor.Validate(); err == nil {
			t.Fatalf("invalid cursor accepted: %#v", cursor)
		}
	}
}

func TestParseQueryRejectsDuplicateFields(t *testing.T) {
	input := `{"schema_version":1,"operation":"summary","operation":"events","start":"2026-08-01T00:00:00Z","end":"2026-08-02T00:00:00Z","time_zone":"UTC"}`
	if _, err := ParseQuery([]byte(input)); err == nil || !strings.Contains(err.Error(), "duplicate object field") {
		t.Fatalf("duplicate operation returned %v", err)
	}
}

func TestLimitWindowLifetimeHasNoReset(t *testing.T) {
	limit := KeyLimit{KeyID: keyIDA, Window: LimitWindowLifetime, NextResetAt: nil}
	data, err := json.Marshal(limit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"window":"lifetime"`) || !strings.Contains(string(data), `"next_reset_at":null`) {
		t.Fatalf("lifetime limit JSON = %s", data)
	}
}

func TestLeaderboardStableTieBreaker(t *testing.T) {
	rows := []LeaderboardRow{
		{KeyID: keyIDB, Tokens: TokenUsage{Total: 200}, KnownCost: 3_000_000_000},
		{KeyID: keyIDA, Tokens: TokenUsage{Total: 200}, KnownCost: 3_000_000_000},
		{KeyID: strings.Repeat("f", 64), Tokens: TokenUsage{Total: 300}, KnownCost: 2_000_000_000},
	}
	SortLeaderboard(rows, LeaderboardSortTokens)
	if rows[0].Tokens.Total != 300 || rows[1].KeyID != keyIDA || rows[2].KeyID != keyIDB {
		t.Fatalf("token order = %#v", rows)
	}
	for index := range rows {
		if rows[index].Rank != index+1 {
			t.Fatalf("rank %d = %d", index, rows[index].Rank)
		}
	}
	SortLeaderboard(rows, LeaderboardSortCost)
	if rows[0].KeyID != keyIDA || rows[1].KeyID != keyIDB {
		t.Fatalf("cost tie order = %#v", rows)
	}
}

func TestLeaderboardPagePreservesGlobalRank(t *testing.T) {
	rows := []LeaderboardRow{{KeyID: keyIDB, Tokens: TokenUsage{Total: 200}}}
	SortLeaderboardPage(rows, LeaderboardSortTokens, 2)
	if rows[0].Rank != 3 {
		t.Fatalf("page 2 rank = %d, want 3", rows[0].Rank)
	}
}

func TestViewerDTOsDoNotExposeKeyIdentity(t *testing.T) {
	value := ViewerSummary{Label: "scope-8f7c21", Tokens: TokenUsage{Schema: "normalized-v1", Quality: TokenQualityExact}}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"key_id", "short_key_id", "raw_key", "api_key"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("viewer JSON contains %q: %s", forbidden, data)
		}
	}
}

func TestAnalyticsStatesAreFrozen(t *testing.T) {
	states := []AnalyticsState{StateDisabled, StateStarting, StateReady, StateDegraded, StateCircuitOpen, StateStopping}
	for _, state := range states {
		if !state.Valid() {
			t.Fatalf("state %q is not valid", state)
		}
	}
	if AnalyticsState("unknown").Valid() {
		t.Fatal("unknown state is valid")
	}
}

func validEvent() Event {
	alias := "alias-53b1"
	authType := "oauth"
	credentialID := "58ab8db24f35177352202107ce48e49ac6ea40d79c9f41eddbd90a85f17765dc"
	credentialAlgorithm := CredentialIDAlgorithm
	status := 200
	ttft := int64(23)
	tier := "standard"
	return Event{
		SchemaVersion: EventSchemaVersion, AttemptID: "91a83fb43b38e8770e7648440a89fc48",
		ProxyRequestID: "d1371f43e6b8362d05d7567ed5fcc2ad", RequestIDQuality: RequestIDObserved,
		KeyID: keyIDA, RequestedAt: time.Date(2026, 8, 31, 4, 5, 6, 0, time.UTC),
		Provider: "provider-a17c92", ExecutorType: "executor-47c8", Model: "model-f93b",
		RequestedAlias: &alias, EndpointClass: "responses", AuthType: &authType,
		CredentialID: &credentialID, CredentialIDAlgorithm: &credentialAlgorithm,
		Succeeded: true, UpstreamStatusCode: &status, LatencyMS: 91,
		TimeToFirstTokenMS: &ttft, ServiceTierRequested: &tier, ServiceTierUsed: &tier,
		Generated: true,
		Tokens:    TokenUsage{Input: 70, Output: 30, Reasoning: 10, Cached: 5, CacheRead: 5, Total: 100, Schema: "normalized-v1", Quality: TokenQualityExact},
	}
}

func testCursorCodec(t *testing.T) *CursorCodec {
	t.Helper()
	codec, err := NewCursorCodec([]byte("cursor-fixture-key-32-byte-9f21a"))
	if err != nil {
		t.Fatal(err)
	}
	return codec
}
