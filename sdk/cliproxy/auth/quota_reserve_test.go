package auth

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
)

var quotaReserveBypassID = strings.Repeat("ab", 32)

func TestFableReserveDoesNotHoldOtherModels(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&refreshMockExecutor{id: "claude"})
	auth, errRegister := manager.Register(context.Background(), &Auth{ID: "fable-reserve-isolation", Provider: "claude", Status: StatusActive, Attributes: map[string]string{AttributeQuotaReserve: `{"seven-day-fable":10}`}})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "claude-fable-5-1"}, {ID: "claude-opus-5"}, {ID: "claude-sonnet-5"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	manager.ApplyQuotaReserveBars(auth, []QuotaReserveBar{
		{ID: "five-hour", RemainingPercent: 99},
		{ID: "seven-day", RemainingPercent: 21},
		{ID: "seven-day-fable", RemainingPercent: 0, ResetAt: time.Now().Add(time.Hour)},
	})
	for _, model := range []string{"claude-opus-5", "claude-sonnet-5", "claude-fable-5-1"} {
		ctx, holder := routing.WithTraceHolder(context.Background())
		_, _, errPick := manager.pickNext(ctx, "claude", model, cliproxyexecutor.Options{}, nil)
		if (errPick != nil) != strings.Contains(model, "fable") {
			t.Fatalf("pick %s with Fable-only reserve: %v", model, errPick)
		}
		trace := holder.Last()
		if trace == nil || len(trace.Candidates) != 1 {
			t.Fatalf("missing trace for %s", model)
		}
		if got := trace.Candidates[0].Gate; (got == routing.GateReserve) != strings.Contains(model, "fable") {
			t.Fatalf("reserve gate for %s = %q", model, got)
		}
	}
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"claude": {
			{Name: "claude-fable-5-1", Alias: "family-only"},
			{Name: "claude-opus-5", Alias: "fable-looking-opus"},
		},
	})
	if !manager.quotaReserveHolds(auth, "", "family-only") {
		t.Fatal("Fable alias bypassed its family reserve")
	}
	if manager.quotaReserveHolds(auth, "", "fable-looking-opus") {
		t.Fatal("Opus alias was held based on its display name")
	}
}

func TestEvaluateQuotaReservesHoldAtOrBelowReserve(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	hits := EvaluateQuotaReserves(map[string]float64{"weekly": 30, "five-hour": 10}, []QuotaReserveBar{
		{ID: "weekly", RemainingPercent: 30, ResetAt: reset},
		{ID: "five-hour", RemainingPercent: 11},
		{ID: "uncapped", RemainingPercent: 0},
	})
	if len(hits) != 1 || hits[0].BarID != "weekly" || !hits[0].ResetAt.Equal(reset) {
		t.Fatalf("hits = %+v, want only weekly", hits)
	}
}

func TestQuotaReserveUntilWaitsForLatestKnownReset(t *testing.T) {
	early, late := time.Now().Add(time.Hour), time.Now().Add(3*time.Hour)
	if got := quotaReserveUntil([]QuotaReserveHit{{ResetAt: early}, {ResetAt: late}}); !got.Equal(late) {
		t.Fatalf("until = %v, want %v", got, late)
	}
	if got := quotaReserveUntil([]QuotaReserveHit{{ResetAt: early}, {}}); !got.IsZero() {
		t.Fatalf("until with unknown reset = %v, want zero", got)
	}
}

func TestNormalizeQuotaReservesRejectsInvalidValues(t *testing.T) {
	for _, value := range []any{
		map[string]any{"weekly": 100.0},
		map[string]any{"weekly": -1.0},
		map[string]any{"": 10.0},
		map[string]any{"weekly": "30"},
		[]any{"weekly"},
	} {
		if _, err := NormalizeQuotaReserves(value); err == nil {
			t.Fatalf("NormalizeQuotaReserves(%v) error = nil", value)
		}
	}
	if _, err := NormalizeQuotaReserveBypassKeys([]any{"sk-raw-key"}); err == nil {
		t.Fatal("raw API key accepted as bypass key id")
	}
	keys, err := NormalizeQuotaReserveBypassKeys([]any{strings.ToUpper(quotaReserveBypassID), quotaReserveBypassID})
	if err != nil || len(keys) != 1 || keys[0] != quotaReserveBypassID {
		t.Fatalf("keys = %v, err = %v", keys, err)
	}
}

func TestApplyQuotaReserveMetadataMirrorsValidMetadataOnly(t *testing.T) {
	auth := &Auth{Metadata: map[string]any{
		AttributeQuotaReserve:           map[string]any{"weekly": 30.0},
		AttributeQuotaReserveBypassKeys: []any{"not-a-key-id"},
	}}
	ApplyQuotaReserveMetadata(auth)
	if got := QuotaReservesForAuth(auth); got["weekly"] != 30 {
		t.Fatalf("reserves = %v", got)
	}
	if _, ok := auth.Attributes[AttributeQuotaReserveBypassKeys]; ok {
		t.Fatal("invalid bypass list mirrored into attributes")
	}
}

// TestQuotaReserveHoldsCredentialUnlessBypassed drives real selection: a
// held credential is skipped for ordinary client keys, served to a bypass
// key, and returns once its bars recover.
func TestQuotaReserveHoldsCredentialUnlessBypassed(t *testing.T) {
	const model = "quota-reserve-test-model"
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&refreshMockExecutor{id: "claude"})
	held := &Auth{
		ID:       "quota-reserve-held",
		Provider: "claude",
		Status:   StatusActive,
		Attributes: map[string]string{
			AttributeQuotaReserve:           `{"weekly":30}`,
			AttributeQuotaReserveBypassKeys: `["` + quotaReserveBypassID + `"]`,
			"priority":                      "10",
		},
	}
	other := &Auth{ID: "quota-reserve-other", Provider: "claude", Status: StatusActive}
	for _, auth := range []*Auth{held, other} {
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, err)
		}
	}

	pick := func(keyID string) string {
		t.Helper()
		opts := cliproxyexecutor.Options{Metadata: map[string]any{}}
		if keyID != "" {
			opts.Metadata[cliproxyexecutor.ClientKeyIDMetadataKey] = keyID
		}
		selected, _, err := manager.pickNext(context.Background(), "claude", model, opts, nil)
		if err != nil {
			t.Fatalf("pickNext error = %v", err)
		}
		return selected.ID
	}

	if got := pick(""); got != held.ID {
		t.Fatalf("before reserve: picked %s, want higher-priority %s", got, held.ID)
	}
	live, _ := manager.GetByID(held.ID)
	manager.ApplyQuotaReserveBars(live, []QuotaReserveBar{{ID: "weekly", RemainingPercent: 20, ResetAt: time.Now().Add(time.Hour)}})
	if got := pick(""); got != other.ID {
		t.Fatalf("held, no key: picked %s, want %s", got, other.ID)
	}
	if got := pick(strings.Repeat("cd", 32)); got != other.ID {
		t.Fatalf("held, other key: picked %s, want %s", got, other.ID)
	}
	if got := pick(quotaReserveBypassID); got != held.ID {
		t.Fatalf("held, bypass key: picked %s, want %s", got, held.ID)
	}
	manager.ApplyQuotaReserveBars(live, []QuotaReserveBar{{ID: "weekly", RemainingPercent: 90}})
	if got := pick(""); got != held.ID {
		t.Fatalf("after recovery: picked %s, want %s", got, held.ID)
	}
}

func TestQuotaReserveStateExpiresAtReset(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	now := time.Now()
	tracker := manager.quotaReserves()
	tracker.now = func() time.Time { return now }
	auth := &Auth{ID: "a", Attributes: map[string]string{AttributeQuotaReserve: `{"weekly":30}`}}
	manager.ApplyQuotaReserveBars(auth, []QuotaReserveBar{{ID: "weekly", RemainingPercent: 10, ResetAt: now.Add(time.Minute)}})
	if !manager.quotaReserveHolds(auth, "") {
		t.Fatal("credential not held")
	}
	now = now.Add(2 * time.Minute)
	if manager.quotaReserveHolds(auth, "") || manager.hasHeldQuotaReserve() {
		t.Fatal("reserve hold outlived its reset")
	}
}

func TestScheduleQuotaReserveCheckThrottlesPerCredential(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "throttle", Provider: "claude", Status: StatusActive, Attributes: map[string]string{AttributeQuotaReserve: `{"weekly":30}`}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	done := make(chan struct{}, 4)
	manager.SetQuotaReserveProber(func(context.Context, *Auth) ([]QuotaReserveBar, error) {
		calls.Add(1)
		done <- struct{}{}
		return []QuotaReserveBar{{ID: "weekly", RemainingPercent: 5}}, nil
	})
	manager.scheduleQuotaReserveCheck(auth.ID)
	<-done
	for i := 0; i < 3; i++ {
		manager.scheduleQuotaReserveCheck(auth.ID)
	}
	manager.quotaReserves().mu.Lock()
	inFlight := len(manager.quotaReserves().inFlight)
	manager.quotaReserves().mu.Unlock()
	if calls.Load() != 1 || inFlight > 1 {
		t.Fatalf("prober calls = %d, want 1 within the throttle window", calls.Load())
	}
	// Wait for the background check to finish applying its result.
	for {
		manager.quotaReserves().mu.Lock()
		running := len(manager.quotaReserves().inFlight)
		manager.quotaReserves().mu.Unlock()
		if running == 0 {
			break
		}
		runtime.Gosched()
	}
	// Held with an unknown reset stays held until a later check clears it.
	if !manager.quotaReserveHolds(auth, "") {
		t.Fatal("check result not applied")
	}
}

func TestQuotaReserveEditAppliesToLastObservation(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "edit", Provider: "claude", Status: StatusActive, Attributes: map[string]string{AttributeQuotaReserve: `{"weekly":30}`}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	live, _ := manager.GetByID(auth.ID)
	manager.ApplyQuotaReserveBars(live, []QuotaReserveBar{{ID: "weekly", RemainingPercent: 19, ResetAt: time.Now().Add(time.Hour)}})
	if _, held := manager.QuotaReserveState(auth.ID); !held {
		t.Fatal("not held at 19% remaining with a 30% reserve")
	}
	live.Attributes[AttributeQuotaReserve] = `{"weekly":10}`
	if _, err := manager.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	if _, held := manager.QuotaReserveState(auth.ID); held {
		t.Fatal("still held after lowering the reserve below observed usage")
	}
}

func TestQuotaReserveSettingEditsImmediatelyChangeRouting(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&refreshMockExecutor{id: "claude"})
	const model = "reserve-edit-routing"
	for _, auth := range []*Auth{
		{ID: "reserve-edit-primary", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"priority": "10", AttributeQuotaReserve: `{"weekly":30}`}},
		{ID: "reserve-edit-fallback", Provider: "claude", Status: StatusActive},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}
	auth, _ := manager.GetByID("reserve-edit-primary")
	manager.ApplyQuotaReserveBars(auth, []QuotaReserveBar{{ID: "weekly", RemainingPercent: 20, ResetAt: time.Now().Add(time.Hour)}})
	for _, test := range []struct {
		reserve string
		bypass  string
		want    string
	}{
		{`{"weekly":30}`, "", "reserve-edit-fallback"},
		{`{"weekly":10}`, "", "reserve-edit-primary"},
		{`{"weekly":40}`, "", "reserve-edit-fallback"},
		{`{"weekly":40}`, `["` + quotaReserveBypassID + `"]`, "reserve-edit-primary"},
		{`{}`, "", "reserve-edit-primary"},
		{`{"weekly":40}`, "", "reserve-edit-fallback"},
	} {
		auth, _ = manager.GetByID(auth.ID)
		auth.Attributes[AttributeQuotaReserve] = test.reserve
		auth.Attributes[AttributeQuotaReserveBypassKeys] = test.bypass
		if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
			t.Fatal(errUpdate)
		}
		manager.RecheckQuotaReserve(auth.ID)
		selected, _, errPick := manager.pickNext(context.Background(), "claude", model, cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.ClientKeyIDMetadataKey: quotaReserveBypassID}}, nil)
		if errPick != nil || selected.ID != test.want {
			t.Fatalf("reserve %s bypass %s: selected %v, error %v, want %s", test.reserve, test.bypass, selected, errPick, test.want)
		}
	}
}

func TestRoutingTraceNamesQuotaReserveGate(t *testing.T) {
	const model = "quota-reserve-trace-model"
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&refreshMockExecutor{id: "claude"})
	held := &Auth{
		ID:         "quota-reserve-trace-held",
		Provider:   "claude",
		Status:     StatusActive,
		Attributes: map[string]string{AttributeQuotaReserve: `{"weekly":30}`},
	}
	other := &Auth{ID: "quota-reserve-trace-other", Provider: "claude", Status: StatusActive}
	for _, auth := range []*Auth{held, other} {
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, err)
		}
	}
	resetAt := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	live, _ := manager.GetByID(held.ID)
	manager.ApplyQuotaReserveBars(live, []QuotaReserveBar{{ID: "weekly", RemainingPercent: 20, ResetAt: resetAt}})

	heldGate := func(ctx context.Context) routing.Candidate {
		t.Helper()
		ctx, holder := routing.WithTraceHolder(ctx)
		_, _, _ = manager.pickNext(ctx, "claude", model, cliproxyexecutor.Options{Metadata: map[string]any{}}, nil)
		trace := holder.Last()
		if trace == nil {
			t.Fatal("no routing trace captured")
		}
		for _, candidate := range trace.Candidates {
			if candidate.ID == held.ID {
				return candidate
			}
		}
		t.Fatalf("held credential missing from trace: %+v", trace.Candidates)
		return routing.Candidate{}
	}

	if got := heldGate(context.Background()); got.Gate != routing.GateReserve || got.UntilMs != resetAt.UnixMilli() {
		t.Fatalf("held candidate = %+v, want gate %q until %d", got, routing.GateReserve, resetAt.UnixMilli())
	}
	// A policy that also rejects the credential outlasts the reserve, so it is named instead.
	if got := heldGate(withRequiredAuthKind(context.Background(), "oauth")); got.Gate != routing.GatePolicy || got.UntilMs != 0 {
		t.Fatalf("policy and reserve candidate = %+v, want gate %q", got, routing.GatePolicy)
	}
}

func TestQuotaReservePartialObservationsMergeAndExpire(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), &Auth{ID: "partial-reserve", Provider: "claude", Status: StatusActive, Attributes: map[string]string{AttributeQuotaReserve: `{"seven-day-fable":10,"seven-day":30}`}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	manager.quotaReserves().now = func() time.Time { return now }
	manager.ApplyQuotaReserveBars(auth, []QuotaReserveBar{{ID: "seven-day-fable", RemainingPercent: 5, ResetAt: now.Add(time.Hour)}})
	manager.ApplyQuotaReserveBars(auth, []QuotaReserveBar{{ID: "seven-day", RemainingPercent: 90}})
	if !manager.quotaReserveHolds(auth, "", "claude-fable-5-1") || manager.quotaReserveHolds(auth, "", "claude-opus-5") {
		t.Fatal("partial observation lost family hold or held another family")
	}
	auth.Attributes[AttributeQuotaReserve] = `{"seven-day-fable":4,"seven-day":30}`
	if manager.quotaReserveHolds(auth, "", "claude-fable-5-1") {
		t.Fatal("reserve edit did not immediately apply")
	}
	auth.Attributes[AttributeQuotaReserve] = `{"seven-day-fable":10,"seven-day":30}`
	auth.Attributes[AttributeQuotaReserveBypassKeys] = `["` + quotaReserveBypassID + `"]`
	if manager.quotaReserveHolds(auth, quotaReserveBypassID, "claude-fable-5-1") {
		t.Fatal("bypass edit did not immediately apply")
	}
	now = now.Add(time.Hour)
	if manager.quotaReserveHolds(auth, "", "claude-fable-5-1") {
		t.Fatal("expired unreported window remained held")
	}
	manager.ApplyQuotaReserveBars(auth, []QuotaReserveBar{{ID: "seven-day-fable", RemainingPercent: 5, ResetAt: now.Add(time.Hour)}})
	manager.InvalidateQuotaReserveObservation(auth.ID, []string{"seven-day-fable"})
	manager.ApplyQuotaReserveBars(auth, []QuotaReserveBar{{ID: "seven-day", RemainingPercent: 90}})
	if manager.quotaReserveHolds(auth, "", "claude-fable-5-1") {
		t.Fatal("invalidated window returned on a partial read")
	}
	manager.ApplyQuotaReserveBars(auth, []QuotaReserveBar{{ID: "seven-day-fable", RemainingPercent: 5}})
	manager.ApplyQuotaReserveBars(auth, []QuotaReserveBar{{ID: "seven-day-fable", RemainingPercent: 90}})
	if manager.quotaReserveHolds(auth, "", "claude-fable-5-1") {
		t.Fatal("new value did not replace same window")
	}
}
