package auth

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var usageCapBypassID = strings.Repeat("ab", 32)

func TestEvaluateUsageCapsBlocksAtOrBelowCap(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	hits := EvaluateUsageCaps(map[string]float64{"weekly": 30, "five-hour": 10}, []UsageCapBar{
		{ID: "weekly", RemainingPercent: 30, ResetAt: reset},
		{ID: "five-hour", RemainingPercent: 11},
		{ID: "uncapped", RemainingPercent: 0},
	})
	if len(hits) != 1 || hits[0].BarID != "weekly" || !hits[0].ResetAt.Equal(reset) {
		t.Fatalf("hits = %+v, want only weekly", hits)
	}
}

func TestUsageCapUntilWaitsForLatestKnownReset(t *testing.T) {
	early, late := time.Now().Add(time.Hour), time.Now().Add(3*time.Hour)
	if got := usageCapUntil([]UsageCapHit{{ResetAt: early}, {ResetAt: late}}); !got.Equal(late) {
		t.Fatalf("until = %v, want %v", got, late)
	}
	if got := usageCapUntil([]UsageCapHit{{ResetAt: early}, {}}); !got.IsZero() {
		t.Fatalf("until with unknown reset = %v, want zero", got)
	}
}

func TestNormalizeUsageCapsRejectsInvalidValues(t *testing.T) {
	for _, value := range []any{
		map[string]any{"weekly": 100.0},
		map[string]any{"weekly": -1.0},
		map[string]any{"": 10.0},
		map[string]any{"weekly": "30"},
		[]any{"weekly"},
	} {
		if _, err := NormalizeUsageCaps(value); err == nil {
			t.Fatalf("NormalizeUsageCaps(%v) error = nil", value)
		}
	}
	if _, err := NormalizeUsageCapBypassKeys([]any{"sk-raw-key"}); err == nil {
		t.Fatal("raw API key accepted as bypass key id")
	}
	keys, err := NormalizeUsageCapBypassKeys([]any{strings.ToUpper(usageCapBypassID), usageCapBypassID})
	if err != nil || len(keys) != 1 || keys[0] != usageCapBypassID {
		t.Fatalf("keys = %v, err = %v", keys, err)
	}
}

func TestApplyUsageCapMetadataMirrorsValidMetadataOnly(t *testing.T) {
	auth := &Auth{Metadata: map[string]any{
		AttributeUsageCaps:          map[string]any{"weekly": 30.0},
		AttributeUsageCapBypassKeys: []any{"not-a-key-id"},
	}}
	ApplyUsageCapMetadata(auth)
	if got := UsageCapsForAuth(auth); got["weekly"] != 30 {
		t.Fatalf("caps = %v", got)
	}
	if _, ok := auth.Attributes[AttributeUsageCapBypassKeys]; ok {
		t.Fatal("invalid bypass list mirrored into attributes")
	}
}

// TestUsageCapWithholdsCredentialUnlessBypassed drives real selection: a
// capped credential is skipped for ordinary client keys, served to a bypass
// key, and returns once its bars recover.
func TestUsageCapWithholdsCredentialUnlessBypassed(t *testing.T) {
	const model = "usage-cap-test-model"
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&refreshMockExecutor{id: "claude"})
	capped := &Auth{
		ID:       "usage-cap-capped",
		Provider: "claude",
		Status:   StatusActive,
		Attributes: map[string]string{
			AttributeUsageCaps:          `{"weekly":30}`,
			AttributeUsageCapBypassKeys: `["` + usageCapBypassID + `"]`,
			"priority":                  "10",
		},
	}
	other := &Auth{ID: "usage-cap-other", Provider: "claude", Status: StatusActive}
	for _, auth := range []*Auth{capped, other} {
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

	if got := pick(""); got != capped.ID {
		t.Fatalf("before cap: picked %s, want higher-priority %s", got, capped.ID)
	}
	live, _ := manager.GetByID(capped.ID)
	manager.ApplyUsageCapBars(live, []UsageCapBar{{ID: "weekly", RemainingPercent: 20, ResetAt: time.Now().Add(time.Hour)}})
	if got := pick(""); got != other.ID {
		t.Fatalf("capped, no key: picked %s, want %s", got, other.ID)
	}
	if got := pick(strings.Repeat("cd", 32)); got != other.ID {
		t.Fatalf("capped, other key: picked %s, want %s", got, other.ID)
	}
	if got := pick(usageCapBypassID); got != capped.ID {
		t.Fatalf("capped, bypass key: picked %s, want %s", got, capped.ID)
	}
	manager.ApplyUsageCapBars(live, []UsageCapBar{{ID: "weekly", RemainingPercent: 90}})
	if got := pick(""); got != capped.ID {
		t.Fatalf("after recovery: picked %s, want %s", got, capped.ID)
	}
}

func TestUsageCapStateExpiresAtReset(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	now := time.Now()
	tracker := manager.usageCaps()
	tracker.now = func() time.Time { return now }
	auth := &Auth{ID: "a", Attributes: map[string]string{AttributeUsageCaps: `{"weekly":30}`}}
	manager.ApplyUsageCapBars(auth, []UsageCapBar{{ID: "weekly", RemainingPercent: 10, ResetAt: now.Add(time.Minute)}})
	if !manager.usageCappedFor(auth, "") {
		t.Fatal("credential not capped")
	}
	now = now.Add(2 * time.Minute)
	if manager.usageCappedFor(auth, "") || manager.hasActiveUsageCaps() {
		t.Fatal("cap outlived its reset")
	}
}

func TestScheduleUsageCapCheckThrottlesPerCredential(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "throttle", Provider: "claude", Status: StatusActive, Attributes: map[string]string{AttributeUsageCaps: `{"weekly":30}`}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	done := make(chan struct{}, 4)
	manager.SetUsageCapProber(func(context.Context, *Auth) ([]UsageCapBar, error) {
		calls.Add(1)
		done <- struct{}{}
		return []UsageCapBar{{ID: "weekly", RemainingPercent: 5}}, nil
	})
	manager.scheduleUsageCapCheck(auth.ID)
	<-done
	for i := 0; i < 3; i++ {
		manager.scheduleUsageCapCheck(auth.ID)
	}
	manager.usageCaps().mu.Lock()
	inFlight := len(manager.usageCaps().inFlight)
	manager.usageCaps().mu.Unlock()
	if calls.Load() != 1 || inFlight > 1 {
		t.Fatalf("prober calls = %d, want 1 within the throttle window", calls.Load())
	}
	// Wait for the background check to finish applying its result.
	for {
		manager.usageCaps().mu.Lock()
		running := len(manager.usageCaps().inFlight)
		manager.usageCaps().mu.Unlock()
		if running == 0 {
			break
		}
		runtime.Gosched()
	}
	// Capped with an unknown reset stays capped until a later check clears it.
	if !manager.usageCappedFor(auth, "") {
		t.Fatal("check result not applied")
	}
}

func TestUsageCapEditAppliesToLastObservation(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "edit", Provider: "claude", Status: StatusActive, Attributes: map[string]string{AttributeUsageCaps: `{"weekly":30}`}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	live, _ := manager.GetByID(auth.ID)
	manager.ApplyUsageCapBars(live, []UsageCapBar{{ID: "weekly", RemainingPercent: 19, ResetAt: time.Now().Add(time.Hour)}})
	if _, capped := manager.UsageCapState(auth.ID); !capped {
		t.Fatal("not capped at 19% remaining with a 30% cap")
	}
	live.Attributes[AttributeUsageCaps] = `{"weekly":10}`
	if _, err := manager.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	if _, capped := manager.UsageCapState(auth.ID); capped {
		t.Fatal("still capped after lowering the cap below observed usage")
	}
}
