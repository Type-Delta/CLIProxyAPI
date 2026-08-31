package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var fixtureEmailPattern = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)

type derivedFixtureUsage struct {
	Requests map[string]struct{}
	Attempts []string
	Tokens   fixtureTokens
	Cost     NanoUSD
	Unpriced int64
}

func TestFixturesAreValidJSONAndPrivacySafe(t *testing.T) {
	root := filepath.Join("..", "testdata", "upstream-v1.15.0")
	entries := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		entries++
		data, errRead := os.ReadFile(path)
		if errRead != nil {
			return errRead
		}
		var value any
		if errJSON := json.Unmarshal(data, &value); errJSON != nil {
			t.Errorf("%s: invalid JSON: %v", path, errJSON)
			return nil
		}
		if errStrict := validateFixtureContract(filepath.Base(path), data); errStrict != nil {
			t.Errorf("%s: contract validation: %v", path, errStrict)
		}
		lower := strings.ToLower(string(data))
		if strings.Contains(lower, "://") || fixtureEmailPattern.Match(data) {
			t.Errorf("%s: contains a URL or email address", path)
		}
		assertFixturePrivacy(t, path, value, strings.HasPrefix(filepath.Base(path), "viewer_"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if entries < 12 {
		t.Fatalf("found %d JSON fixtures, want at least 12", entries)
	}
}

func TestFixtureManifestHashesEveryContractFile(t *testing.T) {
	root := filepath.Join("..", "testdata", "upstream-v1.15.0")
	data, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		FixtureSchemaVersion int               `json:"fixture_schema_version"`
		Source               string            `json:"source"`
		SourceSHA            string            `json:"source_sha"`
		HashAlgorithm        string            `json:"hash_algorithm"`
		ManifestExcludesSelf bool              `json:"manifest_excludes_self"`
		Files                map[string]string `json:"files"`
	}
	if err := decodeStrictFixture(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.FixtureSchemaVersion != 1 || manifest.SourceSHA != "696a4659ce1d5d6f2d2d0530e3205eb51fbce889" || manifest.HashAlgorithm != "sha256" || !manifest.ManifestExcludesSelf {
		t.Fatalf("invalid fixture manifest metadata: %#v", manifest)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || entry.Name() == "manifest.json" {
			continue
		}
		seen++
		content, errRead := os.ReadFile(filepath.Join(root, entry.Name()))
		if errRead != nil {
			t.Fatal(errRead)
		}
		sum := sha256.Sum256(content)
		if got, want := hex.EncodeToString(sum[:]), manifest.Files[entry.Name()]; got != want {
			t.Errorf("%s digest = %s, want %s", entry.Name(), got, want)
		}
	}
	if seen != len(manifest.Files) {
		t.Fatalf("manifest has %d files, directory has %d contract files", len(manifest.Files), seen)
	}
}

func TestEventFixtureMatchesEventV1(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "testdata", "upstream-v1.15.0", "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Events []Event `json:"events"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Events) != 3 {
		t.Fatalf("event count = %d, want 3", len(fixture.Events))
	}
	requests := map[string]struct{}{}
	for _, event := range fixture.Events {
		if err := event.Validate(); err != nil {
			t.Fatalf("event %s: %v", event.AttemptID, err)
		}
		requests[event.ProxyRequestID] = struct{}{}
	}
	if len(requests) != 2 {
		t.Fatalf("proxy request count = %d, want 2", len(requests))
	}
}

func TestFixtureTotalsReconcileFromEventRows(t *testing.T) {
	root := filepath.Join("..", "testdata", "upstream-v1.15.0")
	data, err := os.ReadFile(filepath.Join(root, "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	var eventsFixture struct {
		SchemaVersion    int     `json:"schema_version"`
		ProxyRequests    int64   `json:"proxy_requests"`
		UpstreamAttempts int64   `json:"upstream_attempts"`
		Events           []Event `json:"events"`
	}
	if err := decodeStrictFixture(data, &eventsFixture); err != nil {
		t.Fatal(err)
	}
	inputPrice, _ := ParseNanoUSD("10.035")
	outputPrice, _ := ParseNanoUSD("15.0525")
	requests := map[string]struct{}{}
	var totalTokens, unpricedTokens int64
	var knownCost NanoUSD
	perKey := map[string]derivedFixtureUsage{}
	for _, event := range eventsFixture.Events {
		requests[event.ProxyRequestID] = struct{}{}
		totalTokens += event.Tokens.Total
		row := perKey[event.KeyID]
		if row.Requests == nil {
			row.Requests = map[string]struct{}{}
		}
		row.Requests[event.ProxyRequestID] = struct{}{}
		row.Attempts = append(row.Attempts, event.AttemptID)
		addFixtureTokens(&row.Tokens, event.Tokens)
		if event.Model == "model-f93b" {
			chargedInput := event.Tokens.Input - event.Tokens.CacheRead - event.Tokens.CacheCreation
			if chargedInput < 0 {
				chargedInput = 0
			}
			inputCost, errCost := CostForTokens(chargedInput, inputPrice)
			if errCost != nil {
				t.Fatal(errCost)
			}
			outputCost, errCost := CostForTokens(event.Tokens.Output, outputPrice)
			if errCost != nil {
				t.Fatal(errCost)
			}
			row.Cost += inputCost + outputCost
			knownCost += inputCost + outputCost
		} else {
			row.Unpriced += event.Tokens.Total
			unpricedTokens += event.Tokens.Total
		}
		perKey[event.KeyID] = row
	}
	if int64(len(requests)) != eventsFixture.ProxyRequests || int64(len(eventsFixture.Events)) != eventsFixture.UpstreamAttempts {
		t.Fatalf("event request/attempt totals do not reconcile")
	}

	data, err = os.ReadFile(filepath.Join(root, "reconciliation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var reconciliation struct {
		Overall struct {
			ProxyRequests    int64   `json:"proxy_requests"`
			UpstreamAttempts int64   `json:"upstream_attempts"`
			TotalTokens      int64   `json:"total_tokens"`
			KnownCost        NanoUSD `json:"known_cost_usd"`
			UnpricedTokens   int64   `json:"unpriced_tokens"`
		} `json:"overall"`
	}
	if err := json.Unmarshal(data, &reconciliation); err != nil {
		t.Fatal(err)
	}
	got := reconciliation.Overall
	if got.ProxyRequests != int64(len(requests)) || got.UpstreamAttempts != int64(len(eventsFixture.Events)) || got.TotalTokens != totalTokens || got.KnownCost != knownCost || got.UnpricedTokens != unpricedTokens {
		t.Fatalf("reconciliation overall = %#v; events give requests=%d attempts=%d tokens=%d cost=%s unpriced=%d", got, len(requests), len(eventsFixture.Events), totalTokens, knownCost, unpricedTokens)
	}
	if len(perKey) != 3 {
		t.Fatalf("per-key event groups = %d, want 3", len(perKey))
	}
	assertSemanticFixtureJoins(t, root, eventsFixture.Events, perKey)
}

func assertSemanticFixtureJoins(t *testing.T, root string, events []Event, perKey map[string]derivedFixtureUsage) {
	t.Helper()
	reconciliation := readStrictFixture[reconciliationFixture](t, filepath.Join(root, "reconciliation.json"))
	for _, row := range reconciliation.PerKey {
		derived, ok := perKey[row.KeyID]
		if !ok {
			t.Errorf("reconciliation key %s has no event rows", row.KeyID)
			continue
		}
		assertFixtureAggregate(t, "per_key "+row.KeyID, row.fixtureAggregate, derived)
	}
	all := deriveFixtureUsage(events)
	assertFixtureAggregate(t, "multi_key", reconciliation.MultiKey.fixtureAggregate, all)
	if len(reconciliation.MultiKey.KeyIDs) != len(perKey) {
		t.Errorf("multi-key IDs = %d, want %d", len(reconciliation.MultiKey.KeyIDs), len(perKey))
	}
	for _, view := range reconciliation.Views {
		selected := filterFixtureEvents(events, view.KeyIDs)
		derived := deriveFixtureUsage(selected)
		if view.Summary.ProxyRequests != int64(len(derived.Requests)) || view.Summary.UpstreamAttempts != int64(len(derived.Attempts)) || view.Summary.Tokens != derived.Tokens || view.Summary.KnownCost != derived.Cost || view.Summary.UnpricedTokens != derived.Unpriced {
			t.Errorf("view %s summary does not match selected events", view.Name)
		}
		if len(view.Timeseries) != 1 || view.Timeseries[0].ProxyRequests != int64(len(derived.Requests)) || view.Timeseries[0].UpstreamAttempts != int64(len(derived.Attempts)) || view.Timeseries[0].TotalTokens != derived.Tokens.Total || view.Timeseries[0].KnownCost != derived.Cost || view.Timeseries[0].UnpricedTokens != derived.Unpriced {
			t.Errorf("view %s timeseries does not match selected events", view.Name)
		}
		if len(view.Dimensions) != 1 || view.Dimensions[0].TotalTokens != derived.Tokens.Total || view.Dimensions[0].KnownCost != derived.Cost || view.Dimensions[0].UnpricedTokens != derived.Unpriced {
			t.Errorf("view %s dimensions do not match selected events", view.Name)
		}
		if !slicesEqual(view.Events, derived.Attempts) {
			t.Errorf("view %s events = %v, want %v", view.Name, view.Events, derived.Attempts)
		}
	}

	tokenFixture := readStrictFixture[tokenCategoriesFixture](t, filepath.Join(root, "token_categories.json"))
	foundAll := false
	for _, vector := range tokenFixture.Cases {
		if vector.Name == "all_categories" {
			foundAll = true
			if vector.fixtureTokens != all.Tokens {
				t.Errorf("all token categories = %#v, want %#v", vector.fixtureTokens, all.Tokens)
			}
		}
	}
	if !foundAll {
		t.Error("all_categories token vector is missing")
	}

	keys := readStrictFixture[struct {
		SchemaVersion int           `json:"schema_version"`
		Catalog       []KeyIdentity `json:"catalog"`
		Limits        []KeyLimit    `json:"limits"`
	}](t, filepath.Join(root, "keys_limits.json"))
	for _, key := range keys.Catalog {
		derived := perKey[key.KeyID]
		if key.TotalTokens != derived.Tokens.Total || key.KnownCost != derived.Cost || key.UnpricedTokens != derived.Unpriced {
			t.Errorf("key catalog %s does not match events", key.KeyID)
		}
	}
	for _, limit := range keys.Limits {
		derived := perKey[limit.KeyID]
		if limit.RequestsConsumed != int64(len(derived.Requests)) || limit.TokensConsumed != derived.Tokens.Total {
			t.Errorf("limit consumption %s does not match events", limit.KeyID)
		}
	}

	leaderboard := readStrictFixture[leaderboardFixture](t, filepath.Join(root, "leaderboard.json"))
	expected := make([]LeaderboardRow, 0, len(perKey))
	shortIDs, err := ShortKeyIDs(reconciliation.MultiKey.KeyIDs)
	if err != nil {
		t.Fatal(err)
	}
	for keyID, derived := range perKey {
		expected = append(expected, LeaderboardRow{KeyID: keyID, ShortKeyID: shortIDs[keyID], Tokens: TokenUsage{Total: derived.Tokens.Total}, KnownCost: derived.Cost, UnpricedTokens: derived.Unpriced})
	}
	assertLeaderboardFixture(t, "tokens", leaderboard.Tokens, expected, LeaderboardSortTokens)
	assertLeaderboardFixture(t, "cost", leaderboard.Cost, expected, LeaderboardSortCost)
	if len(leaderboard.Pagination.TokensPage2) != 1 || leaderboard.Pagination.TokensPage2[0].Rank != 3 || leaderboard.Pagination.Cursor.Rank != 2 {
		t.Errorf("leaderboard pagination does not preserve global ranks")
	}

	viewer := readStrictFixture[struct {
		Capabilities ViewerCapabilities `json:"capabilities"`
		Summary      ViewerSummary      `json:"summary"`
		Events       ViewerEventPage    `json:"events"`
	}](t, filepath.Join(root, "viewer_scope.json"))
	if len(viewer.Events.Events) != 1 {
		t.Fatalf("viewer event count = %d, want 1", len(viewer.Events.Events))
	}
	viewerEvent := viewer.Events.Events[0]
	var source *Event
	for index := range events {
		if events[index].AttemptID == viewerEvent.AttemptID {
			source = &events[index]
			break
		}
	}
	if source == nil {
		t.Fatalf("viewer event %s has no source event", viewerEvent.AttemptID)
	}
	viewerCost := perKey[source.KeyID].Cost
	if viewerEvent.ProxyRequestID != source.ProxyRequestID || viewerEvent.Succeeded != source.Succeeded || !equalIntPointers(viewerEvent.UpstreamStatusCode, source.UpstreamStatusCode) || !equalStringPointers(viewerEvent.ErrorClass, source.ErrorClass) || viewerEvent.Tokens != source.Tokens || viewerEvent.KnownCost == nil || *viewerEvent.KnownCost != viewerCost {
		t.Errorf("viewer event does not match source event %s", source.AttemptID)
	}
	derivedViewer := deriveFixtureUsage([]Event{*source})
	if viewer.Summary.ProxyRequests != int64(len(derivedViewer.Requests)) || viewer.Summary.UpstreamAttempts != 1 || viewer.Summary.Tokens.Total != derivedViewer.Tokens.Total || viewer.Summary.KnownCost != derivedViewer.Cost || viewer.Summary.UnpricedTokens != derivedViewer.Unpriced {
		t.Error("viewer summary does not match its implicit single-key event slice")
	}
}

func deriveFixtureUsage(events []Event) derivedFixtureUsage {
	usage := derivedFixtureUsage{Requests: map[string]struct{}{}}
	inputPrice, _ := ParseNanoUSD("10.035")
	outputPrice, _ := ParseNanoUSD("15.0525")
	for _, event := range events {
		usage.Requests[event.ProxyRequestID] = struct{}{}
		usage.Attempts = append(usage.Attempts, event.AttemptID)
		addFixtureTokens(&usage.Tokens, event.Tokens)
		if event.Model != "model-f93b" {
			usage.Unpriced += event.Tokens.Total
			continue
		}
		chargedInput := event.Tokens.Input - event.Tokens.CacheRead - event.Tokens.CacheCreation
		if chargedInput < 0 {
			chargedInput = 0
		}
		inputCost, _ := CostForTokens(chargedInput, inputPrice)
		outputCost, _ := CostForTokens(event.Tokens.Output, outputPrice)
		usage.Cost += inputCost + outputCost
	}
	return usage
}

func filterFixtureEvents(events []Event, keyIDs []string) []Event {
	selected := map[string]struct{}{}
	for _, keyID := range keyIDs {
		selected[keyID] = struct{}{}
	}
	result := make([]Event, 0, len(events))
	for _, event := range events {
		if _, ok := selected[event.KeyID]; ok {
			result = append(result, event)
		}
	}
	return result
}

func addFixtureTokens(target *fixtureTokens, source TokenUsage) {
	target.Input += source.Input
	target.Output += source.Output
	target.Reasoning += source.Reasoning
	target.Cached += source.Cached
	target.CacheRead += source.CacheRead
	target.CacheCreation += source.CacheCreation
	target.Total += source.Total
}

func assertFixtureAggregate(t *testing.T, name string, got fixtureAggregate, want derivedFixtureUsage) {
	t.Helper()
	if got.ProxyRequests != int64(len(want.Requests)) || got.UpstreamAttempts != int64(len(want.Attempts)) || got.TotalTokens != want.Tokens.Total || got.KnownCost != want.Cost || got.UnpricedTokens != want.Unpriced {
		t.Errorf("%s does not reconcile with events", name)
	}
}

func assertLeaderboardFixture(t *testing.T, name string, got []leaderboardFixtureRow, rows []LeaderboardRow, sortBy LeaderboardSort) {
	t.Helper()
	expected := append([]LeaderboardRow(nil), rows...)
	SortLeaderboard(expected, sortBy)
	if len(got) != len(expected) {
		t.Errorf("%s leaderboard rows = %d, want %d", name, len(got), len(expected))
		return
	}
	var percentTotal NanoUSD
	for index := range expected {
		if got[index].Rank != expected[index].Rank || got[index].KeyID != expected[index].KeyID || got[index].ShortKeyID != expected[index].ShortKeyID || got[index].TotalTokens != expected[index].Tokens.Total || got[index].KnownCost != expected[index].KnownCost || got[index].UnpricedTokens != expected[index].UnpricedTokens {
			t.Errorf("%s leaderboard row %d does not match events", name, index)
		}
		percent, err := ParseNanoUSD(got[index].Percent)
		if err != nil {
			t.Errorf("%s leaderboard percent %q: %v", name, got[index].Percent, err)
		} else {
			percentTotal += percent
		}
	}
	wantPercent, _ := ParseNanoUSD("100")
	if percentTotal != wantPercent {
		t.Errorf("%s leaderboard percentages sum to %s, want 100", name, percentTotal)
	}
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalIntPointers(left, right *int) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func equalStringPointers(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func readStrictFixture[T any](t *testing.T, path string) T {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result T
	if err := decodeStrictFixture(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCredentialAndShortIdentityFixtures(t *testing.T) {
	root := filepath.Join("..", "testdata", "upstream-v1.15.0")
	data, err := os.ReadFile(filepath.Join(root, "credential_identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var credentialFixture credentialIdentityFixture
	if err := decodeStrictFixture(data, &credentialFixture); err != nil {
		t.Fatal(err)
	}
	identityKey := []byte("identity-fixture-key-32-byte-2f7")
	fingerprint, err := IdentityKeyFingerprint(identityKey)
	if err != nil || fingerprint != credentialFixture.IdentityKeyFingerprint {
		t.Fatalf("identity fingerprint = %s, %v", fingerprint, err)
	}
	if credentialFixture.Algorithm != CredentialIDAlgorithm || credentialFixture.IdentityEpoch != 1 {
		t.Fatalf("invalid credential fixture algorithm or epoch: %#v", credentialFixture)
	}
	for _, vector := range credentialFixture.Vectors {
		authIndex, authID := "", ""
		if vector.SourceSelector != nil && vector.InputValue != nil {
			if *vector.SourceSelector == "auth_index" {
				authIndex = *vector.InputValue
			} else {
				authID = *vector.InputValue
			}
		}
		if vector.FallbackPresent {
			authID = "fallback-redacted-0d81"
		}
		got, errCredential := CredentialID(identityKey, vector.ProviderInput, authIndex, authID)
		if errCredential != nil || got == nil != (vector.CredentialID == nil) || got != nil && *got != *vector.CredentialID {
			t.Fatalf("credential vector %s = %v, %v; want %v", vector.Name, got, errCredential, vector.CredentialID)
		}
		if vector.InputRedacted && (!IsFullKeyID(vector.InputSHA256) || vector.InputValue == nil || !strings.HasPrefix(*vector.InputValue, "redacted-")) {
			t.Fatalf("credential vector %s does not use a reproducible redacted identity", vector.Name)
		}
	}
	states := map[string]identityKeyStateFixture{}
	for _, state := range credentialFixture.KeyState {
		states[state.Name] = state
	}
	if state := states["matching_fingerprint"]; state.State != StateReady || !state.Available || state.IdentityEpoch != 1 || !state.DatabaseExists || !state.KeyReadable || !state.FingerprintMatches {
		t.Fatalf("matching fingerprint state = %#v", state)
	}
	if state := states["lost_key"]; state.State != StateCircuitOpen || state.Available || state.KeyReadable || state.Recovery != "verified_restore_or_start_new_identity_epoch" {
		t.Fatalf("lost-key state = %#v", state)
	}
	if state := states["new_epoch"]; state.State != StateReady || !state.Available || state.IdentityEpoch != 2 || !state.ArchivesPrevious {
		t.Fatalf("new-epoch state = %#v", state)
	}

	data, err = os.ReadFile(filepath.Join(root, "key_identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var keyFixture keyIdentityFixture
	if err := decodeStrictFixture(data, &keyFixture); err != nil {
		t.Fatal(err)
	}
	for _, vector := range keyFixture.Vectors[:3] {
		got, errShort := ShortKeyIDs(vector.FullIDs)
		if errShort != nil {
			t.Fatalf("short-ID vector %s: %v", vector.Name, errShort)
		}
		for index, fullID := range vector.FullIDs {
			if got[fullID] != vector.DisplayIDs[index] {
				t.Fatalf("short-ID vector %s[%d] = %s, want %s", vector.Name, index, got[fullID], vector.DisplayIDs[index])
			}
		}
	}
	conflict := keyFixture.Vectors[3]
	if conflict.Status != KeyStatusConflict || len(conflict.DistinctSourceFingerprints) != 2 || conflict.RequiredAction != "rotate" {
		t.Fatalf("full-digest conflict fixture = %#v", conflict)
	}
}

func TestLifetimeLimitFixtureHasNoReset(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "testdata", "upstream-v1.15.0", "keys_limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		SchemaVersion int           `json:"schema_version"`
		Catalog       []KeyIdentity `json:"catalog"`
		Limits        []KeyLimit    `json:"limits"`
	}
	if err := decodeStrictFixture(data, &fixture); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, limit := range fixture.Limits {
		if limit.Window == LimitWindowLifetime {
			found = true
			if limit.NextResetAt != nil {
				t.Fatalf("lifetime limit has reset %v", limit.NextResetAt)
			}
		}
	}
	if !found {
		t.Fatal("lifetime limit fixture is missing")
	}
}

func TestErrorFixtureCoversFrozenCodes(t *testing.T) {
	fixture := readStrictFixture[struct {
		Cases []struct {
			Status int           `json:"status"`
			Body   ErrorEnvelope `json:"body"`
		} `json:"cases"`
	}](t, filepath.Join("..", "testdata", "upstream-v1.15.0", "errors.json"))
	want := map[ErrorCode]struct{}{
		ErrorAnalyticsDisabled: {}, ErrorAnalyticsUnavailable: {}, ErrorAnalyticsMaintenance: {},
		ErrorAnalyticsInvalidQuery: {}, ErrorAnalyticsExportTooLarge: {}, ErrorAnalyticsThrottled: {},
		ErrorAnalyticsInternal: {}, ErrorAnalyticsBackupInvalid: {}, ErrorStructuredKeysRequired: {},
	}
	for _, item := range fixture.Cases {
		if _, ok := want[item.Body.Error.Code]; !ok {
			t.Errorf("unexpected error code %q", item.Body.Error.Code)
		}
		delete(want, item.Body.Error.Code)
	}
	if len(want) != 0 {
		t.Errorf("missing frozen error codes: %v", want)
	}
}

func assertFixturePrivacy(t *testing.T, path string, value any, viewer bool) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		for name, child := range typed {
			normalized := strings.ToLower(name)
			forbidden := map[string]bool{
				"key": true, "api_key": true, "raw_key": true, "access_token": true,
				"auth_id": true, "auth_index": true, "request_headers": true,
				"response_headers": true, "request_body": true, "response_body": true,
				"ip_address": true, "forwarded_for": true, "user_agent": true,
			}
			if forbidden[normalized] {
				t.Errorf("%s: forbidden field %q", path, name)
			}
			if viewer && (normalized == "key_id" || normalized == "key_ids" || normalized == "short_key_id") {
				t.Errorf("%s: viewer fixture exposes %q", path, name)
			}
			assertFixturePrivacy(t, path, child, viewer)
		}
	case []any:
		for _, child := range typed {
			assertFixturePrivacy(t, path, child, viewer)
		}
	}
}

func validateFixtureContract(name string, data []byte) error {
	var target any
	switch name {
	case "capabilities.json":
		target = &Capabilities{}
	case "events.json":
		target = &struct {
			SchemaVersion    int     `json:"schema_version"`
			ProxyRequests    int64   `json:"proxy_requests"`
			UpstreamAttempts int64   `json:"upstream_attempts"`
			Events           []Event `json:"events"`
		}{}
	case "keys_limits.json":
		target = &struct {
			SchemaVersion int           `json:"schema_version"`
			Catalog       []KeyIdentity `json:"catalog"`
			Limits        []KeyLimit    `json:"limits"`
		}{}
	case "errors.json":
		target = &struct {
			Cases []struct {
				Status int           `json:"status"`
				Body   ErrorEnvelope `json:"body"`
			} `json:"cases"`
		}{}
	case "viewer_scope.json":
		target = &struct {
			Capabilities ViewerCapabilities `json:"capabilities"`
			Summary      ViewerSummary      `json:"summary"`
			Events       ViewerEventPage    `json:"events"`
		}{}
	case "imports.json":
		target = &struct {
			Create struct {
				SourceKind string  `json:"source_kind"`
				DryRun     bool    `json:"dry_run"`
				Checkpoint *string `json:"checkpoint"`
			} `json:"create"`
			DryRunResult ImportResult `json:"dry_run_result"`
			CommitResult ImportResult `json:"commit_result"`
			Checkpoint   struct {
				BatchID      string `json:"batch_id"`
				Chunk        int64  `json:"chunk"`
				SourceOffset int64  `json:"source_offset"`
				Digest       string `json:"digest"`
			} `json:"checkpoint"`
			Rollback struct {
				BatchID     string `json:"batch_id"`
				RemovedRows int64  `json:"removed_rows"`
				Reconciled  bool   `json:"reconciled"`
			} `json:"rollback"`
		}{}
	case "credential_identity.json":
		target = &credentialIdentityFixture{}
	case "key_identity.json":
		target = &keyIdentityFixture{}
	case "latency_sketch.json":
		target = &latencyFixture{}
	case "leaderboard.json":
		target = &leaderboardFixture{}
	case "maintenance.json":
		target = &struct {
			Create struct {
				Kind    string `json:"kind"`
				Options struct {
					VerifyBackup bool `json:"verify_backup"`
				} `json:"options"`
			} `json:"create"`
			Queued    JobStatus `json:"queued"`
			Running   JobStatus `json:"running"`
			Succeeded JobStatus `json:"succeeded"`
			Canceled  JobStatus `json:"canceled"`
			Failed    JobStatus `json:"failed"`
		}{}
	case "pricing.json":
		target = &pricingFixture{}
	case "query_contracts.json":
		target = &queryContractsFixture{}
	case "ranges.json":
		target = &rangeFixture{}
	case "reconciliation.json":
		target = &reconciliationFixture{}
	case "token_categories.json":
		target = &tokenCategoriesFixture{}
	case "manifest.json":
		target = &struct {
			FixtureSchemaVersion int               `json:"fixture_schema_version"`
			Source               string            `json:"source"`
			SourceSHA            string            `json:"source_sha"`
			HashAlgorithm        string            `json:"hash_algorithm"`
			ManifestExcludesSelf bool              `json:"manifest_excludes_self"`
			Files                map[string]string `json:"files"`
		}{}
	default:
		// These fixtures have heterogeneous vector rows. Duplicate-field
		// rejection plus typed scalar checks below keep them strict without
		// forcing absent fields into null-valued wire data.
		var object map[string]json.RawMessage
		if err := decodeStrictFixture(data, &object); err != nil {
			return err
		}
		if len(object) == 0 {
			return fmt.Errorf("empty fixture object")
		}
		allowed, ok := fixtureRootFields[name]
		if !ok {
			return fmt.Errorf("fixture has no registered contract")
		}
		if len(object) != len(allowed) {
			return fmt.Errorf("root field count = %d, want %d", len(object), len(allowed))
		}
		for field := range object {
			if _, ok := allowed[field]; !ok {
				return fmt.Errorf("unknown root field %q", field)
			}
		}
		return rejectDuplicateJSONFields(data)
	}
	return decodeStrictFixture(data, target)
}

var fixtureRootFields = map[string]map[string]struct{}{
	"credential_identity.json": fields("algorithm", "identity_epoch", "identity_key_fingerprint", "vectors", "key_state"),
	"key_identity.json":        fields("minimum_prefix_length", "prefix_step", "vectors"),
	"latency_sketch.json":      fields("format_version", "relative_error", "sampling_priority", "vectors", "sampling"),
	"leaderboard.json":         fields("range", "tokens", "cost", "tie_rule", "pagination"),
	"maintenance.json":         fields("create", "queued", "running", "succeeded", "canceled", "failed"),
	"pricing.json":             fields("currency_unit", "rounding", "cache_rule", "retry_rule", "precedence", "rules", "vectors"),
	"query_contracts.json":     fields("bounds", "operations", "leaderboard_sorts", "single_key_query", "multi_key_query", "event_cursor_input"),
	"ranges.json":              fields("week_starts_on", "semantics", "cases"),
	"reconciliation.json":      fields("range", "overall", "per_key", "multi_key", "view_reconciliation"),
	"token_categories.json":    fields("accounting_schema", "cases"),
}

func fields(names ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		result[name] = struct{}{}
	}
	return result
}

func decodeStrictFixture(data []byte, target any) error {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}
