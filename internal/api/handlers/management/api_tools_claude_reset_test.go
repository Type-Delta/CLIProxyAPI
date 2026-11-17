package management

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const claudeResetTestURL = "https://api.anthropic.com/api/organizations/11111111-2222-3333-4444-555555555555/reset_rate_limits"

type claudeResetFixture struct {
	handler    *Handler
	auth       *coreauth.Auth
	result     string
	usage      string
	usageCode  int
	catalog    string
	usageCalls atomic.Int32
	claims     atomic.Int32
}

func newClaudeResetFixture(t *testing.T, clears string) *claudeResetFixture {
	t.Helper()
	f := &claudeResetFixture{result: "reset", usageCode: http.StatusServiceUnavailable,
		catalog: fmt.Sprintf(`{"cedar_ember":{"eligible":true,"grants":[{"id":"selected","resets_total":2,"resets_left":2,"clears":%s}]}}`, clears)}
	newGSRProbeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost:
			f.claims.Add(1)
			_, _ = fmt.Fprintf(w, `{"result":%q}`, f.result)
		case r.URL.RawQuery != "":
			_, _ = w.Write([]byte(f.catalog))
		default:
			f.usageCalls.Add(1)
			if f.usageCode == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "120")
			}
			w.WriteHeader(f.usageCode)
			_, _ = w.Write([]byte(f.usage))
		}
	}))
	f.handler, f.auth = gsrProbeHandler(t, "claude", map[string]string{
		coreauth.AttributeQuotaReserve: `{"five-hour":30,"seven-day":30,"seven-day-fable":30,"seven-day-opus":30}`,
	})
	f.handler.authManager.RegisterExecutor(&refreshRecordExecutor{provider: "claude"})
	registry.GetGlobalRegistry().RegisterClient(f.auth.ID, "claude", []*registry.ModelInfo{{ID: "claude-sonnet-5"}, {ID: "claude-fable-5-2"}, {ID: "claude-opus-5"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(f.auth.ID) })
	quotaCall(t, f.handler, nil, f.auth.Index, http.MethodGet, claudeResetGrantsQuotaURL, "", "", true)
	return f
}

func (f *claudeResetFixture) claim(t *testing.T) {
	t.Helper()
	response := quotaCall(t, f.handler, nil, f.auth.Index, http.MethodPost, claudeResetTestURL, "", `{"program":"cedar_ember","grant_id":"selected","request_id":"request-1"}`)
	if response.StatusCode != http.StatusOK || response.Body != fmt.Sprintf(`{"result":%q}`, f.result) {
		t.Fatalf("upstream claim result changed: %+v", response)
	}
}

func (f *claudeResetFixture) blockModel(model string) {
	retry := 48 * time.Hour
	f.handler.authManager.MarkResult(context.Background(), coreauth.Result{AuthID: f.auth.ID, Provider: "claude", Model: model, RetryAfter: &retry, Error: &coreauth.Error{HTTPStatus: 429, Message: "model quota exhausted"}})
}

func (f *claudeResetFixture) hold(bars ...string) {
	observations := make([]coreauth.QuotaReserveBar, 0, len(bars))
	for _, id := range bars {
		observations = append(observations, coreauth.QuotaReserveBar{ID: id, RemainingPercent: 0, ResetAt: time.Now().Add(48 * time.Hour)})
	}
	f.handler.authManager.ApplyQuotaReserveBars(f.auth, observations)
}

func (f *claudeResetFixture) assertSelectable(t *testing.T, model string, selectable bool) {
	t.Helper()
	_, err := f.handler.authManager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if (err == nil) != selectable {
		t.Fatalf("%s selectable=%t, want=%t, error=%v", model, err == nil, selectable, err)
	}
}

func TestAPICallClaudeResetUsesSelectedGrantScope(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%t", full), func(t *testing.T) {
			clears := `["five_hour","seven_day"]`
			if full {
				clears = `["five_hour","seven_day","seven_day_overage_included"]`
			}
			f := newClaudeResetFixture(t, clears)
			f.blockModel("claude-fable-5-2")
			independent, _ := f.handler.authManager.GetByID(f.auth.ID)
			fableDeadline := independent.ModelStates["claude-fable-5-2"].NextRetryAfter
			if _, err := f.handler.authManager.ApplyProviderQuotaReport(context.Background(), f.auth.ID, true, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			f.hold("five-hour", "seven-day", "seven-day-fable")
			f.assertSelectable(t, "claude-sonnet-5", false)
			f.assertSelectable(t, "claude-fable-5-2", false)
			f.claim(t)
			f.assertSelectable(t, "claude-sonnet-5", true)
			f.assertSelectable(t, "claude-fable-5-2", full)
			after, _ := f.handler.authManager.GetByID(f.auth.ID)
			fable := after.ModelStates["claude-fable-5-2"]
			if !full && (!fable.Quota.Exceeded || !fable.NextRetryAfter.Equal(fableDeadline)) || full && fable.Quota.Exceeded {
				t.Fatalf("Fable inference cooldown disagrees with grant scope: %+v", fable)
			}
			state, held := f.handler.authManager.QuotaReserveState(f.auth.ID)
			if full && held || !full && (!held || len(state.Hits) != 1 || state.Hits[0].BarID != "seven-day-fable") {
				t.Fatalf("remaining reserves after reset: held=%t state=%+v", held, state)
			}
		})
	}
}

func TestAPICallClaudeFableResetRequiresFreshSharedQuotaEvidence(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		used int
	}{
		{"read_failed", http.StatusServiceUnavailable, 0},
		{"weekly_still_exhausted", http.StatusOK, 100},
		{"weekly_refund_proves_recovery", http.StatusOK, 30},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newClaudeResetFixture(t, `["five_hour","seven_day_overage_included"]`)
			f.usageCode = test.code
			f.usage = fmt.Sprintf(`{"five_hour":{"utilization":0},"seven_day":{"utilization":%d,"resets_at":%q},"limits":[{"kind":"weekly_scoped","percent":0,"is_active":true,"scope":{"model":{"display_name":"Fable"}}}]}`, test.used, time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339))
			f.blockModel("claude-fable-5-2")
			f.blockModel("claude-opus-5")
			independent, _ := f.handler.authManager.GetByID(f.auth.ID)
			opusDeadline := independent.ModelStates["claude-opus-5"].NextRetryAfter
			if _, err := f.handler.authManager.ApplyProviderQuotaReport(context.Background(), f.auth.ID, true, time.Now().Add(48*time.Hour)); err != nil {
				t.Fatal(err)
			}
			f.hold("five-hour", "seven-day", "seven-day-fable", "seven-day-opus")
			f.claim(t)
			wantRecovered := test.code == http.StatusOK && test.used < 100
			f.assertSelectable(t, "claude-sonnet-5", wantRecovered)
			f.assertSelectable(t, "claude-fable-5-2", wantRecovered)
			f.assertSelectable(t, "claude-opus-5", false)
			after, _ := f.handler.authManager.GetByID(f.auth.ID)
			if state := after.ModelStates["claude-opus-5"]; state.NextRetryAfter.Before(opusDeadline) || wantRecovered && !state.NextRetryAfter.Equal(opusDeadline) {
				t.Fatalf("Fable reset changed independent Opus cooldown: %+v", state)
			}
			if f.usageCalls.Load() != 1 || f.claims.Load() != 1 {
				t.Fatalf("provider calls: usage=%d claims=%d", f.usageCalls.Load(), f.claims.Load())
			}
			state, _ := f.handler.authManager.QuotaReserveState(f.auth.ID)
			for _, hit := range state.Hits {
				if hit.BarID == "seven-day-fable" || hit.BarID == "five-hour" || wantRecovered && hit.BarID == "seven-day" {
					t.Fatalf("refilled reserve still held: %+v", state)
				}
			}
		})
	}
}

func TestAPICallClaudeNotLimitedPreservesReserveAndObservationEpoch(t *testing.T) {
	f := newClaudeResetFixture(t, `["five_hour","seven_day"]`)
	f.result = "not_limited"
	f.blockModel("claude-fable-5-2")
	f.hold("seven-day", "seven-day-fable")
	manager := f.handler.authManager
	observation := manager.CaptureQuotaObservation(context.Background(), f.auth.ID)
	epoch := manager.QuotaObservationEpoch(f.auth.ID)
	before, _ := manager.QuotaReserveState(f.auth.ID)
	f.claim(t)
	after, held := manager.QuotaReserveState(f.auth.ID)
	if manager.QuotaObservationEpoch(f.auth.ID) != epoch || !held || !reflect.DeepEqual(before, after) {
		t.Fatalf("not_limited mutated reset state: epoch=%d want=%d reserve=%+v want=%+v", manager.QuotaObservationEpoch(f.auth.ID), epoch, after, before)
	}
	manager.ApplyQuotaReserveBarsWithContext(observation, f.auth, []coreauth.QuotaReserveBar{{ID: "five-hour", RemainingPercent: 0}})
	state, _ := manager.QuotaReserveState(f.auth.ID)
	if len(state.Hits) != 3 {
		t.Fatalf("not_limited invalidated an in-flight observation: %+v", state)
	}
	f.assertSelectable(t, "claude-fable-5-2", false)
	auth, _ := manager.GetByID(f.auth.ID)
	if !auth.ModelStates["claude-fable-5-2"].Quota.Exceeded {
		t.Fatal("not_limited erased an unrelated Fable inference cooldown")
	}
	if f.usageCalls.Load() != 0 {
		t.Fatal("not_limited performed a reset reconciliation read")
	}
}

func TestAPICallClaudeAlreadyUsedDoesNotResetNewHolds(t *testing.T) {
	f := newClaudeResetFixture(t, `["five_hour","seven_day","seven_day_overage_included"]`)
	f.claim(t)
	f.result = "already_used"
	f.blockModel("claude-fable-5-2")
	f.hold("seven-day-fable")
	epoch := f.handler.authManager.QuotaObservationEpoch(f.auth.ID)
	f.claim(t)
	if f.handler.authManager.QuotaObservationEpoch(f.auth.ID) != epoch {
		t.Fatal("repeated already_used advanced reset epoch")
	}
	f.assertSelectable(t, "claude-fable-5-2", false)
	state, held := f.handler.authManager.QuotaReserveState(f.auth.ID)
	if !held || len(state.Hits) != 1 || !strings.Contains(state.Hits[0].BarID, "fable") {
		t.Fatalf("already_used erased a newer reserve: %+v", state)
	}
}

func TestAPICallClaudeNotLimitedDoesNotProveOtherPoolsHealthy(t *testing.T) {
	for _, clears := range []string{`["five_hour","seven_day_overage_included"]`, `[]`} {
		t.Run(clears, func(t *testing.T) {
			f := newClaudeResetFixture(t, clears)
			f.result = "not_limited"
			f.hold("seven-day", "seven-day-fable")
			if _, err := f.handler.authManager.ApplyProviderQuotaReport(context.Background(), f.auth.ID, true, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			epoch := f.handler.authManager.QuotaObservationEpoch(f.auth.ID)
			before, _ := f.handler.authManager.QuotaReserveState(f.auth.ID)
			f.claim(t)
			f.assertSelectable(t, "claude-sonnet-5", false)
			after, _ := f.handler.authManager.QuotaReserveState(f.auth.ID)
			if f.handler.authManager.QuotaObservationEpoch(f.auth.ID) != epoch || !reflect.DeepEqual(before, after) {
				t.Fatalf("not_limited changed reset epoch or reserve: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestAPICallClaudeUnknownResetScopePreservesIndependentPools(t *testing.T) {
	for _, evidence := range []string{"missing", "expired", "prior_epoch", "malformed"} {
		t.Run(evidence, func(t *testing.T) {
			clears := `["five_hour","seven_day","seven_day_overage_included"]`
			if evidence == "malformed" {
				clears = `null`
			}
			f := newClaudeResetFixture(t, clears)
			switch evidence {
			case "missing":
				f.handler.getQuotaCache().invalidateAuth(quotaAuthIdentityHash(f.auth))
			case "expired":
				future := time.Now().Add(quotaResultCacheTTL + time.Second)
				f.handler.quotaCache.now = func() time.Time { return future }
			case "prior_epoch":
				if _, err := f.handler.authManager.ApplyProviderQuotaReset(context.Background(), f.auth.ID, []string{}, []string{}); err != nil {
					t.Fatal(err)
				}
			}
			f.blockModel("claude-fable-5-2")
			f.blockModel("claude-opus-5")
			f.hold("five-hour", "seven-day", "seven-day-fable", "seven-day-opus")
			before, _ := f.handler.authManager.QuotaReserveState(f.auth.ID)
			f.claim(t)
			f.assertSelectable(t, "claude-fable-5-2", false)
			f.assertSelectable(t, "claude-opus-5", false)
			auth, _ := f.handler.authManager.GetByID(f.auth.ID)
			if !auth.ModelStates["claude-fable-5-2"].Quota.Exceeded || !auth.ModelStates["claude-opus-5"].Quota.Exceeded {
				t.Fatal("unknown reset scope erased an independent inference cooldown")
			}
			after, _ := f.handler.authManager.QuotaReserveState(f.auth.ID)
			if !reflect.DeepEqual(before.Hits, after.Hits) {
				t.Fatalf("unknown scope discarded reserve observations: before=%+v after=%+v", before, after)
			}
			if f.usageCalls.Load() != 1 {
				t.Fatalf("unknown scope fresh reconciliation requests=%d, want=1", f.usageCalls.Load())
			}
		})
	}
}

func TestAPICallClaudeResetReconciliationUsesFreshEpochAndSharedCache(t *testing.T) {
	f := newClaudeResetFixture(t, `["five_hour","seven_day_overage_included"]`)
	f.usageCode = http.StatusOK
	f.usage = fmt.Sprintf(`{"five_hour":{"utilization":100,"resets_at":%q},"seven_day":{"utilization":100,"resets_at":%q}}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339))
	quotaCall(t, f.handler, nil, f.auth.Index, http.MethodGet, claudeUsageQuotaURL, "", "")
	f.hold("five-hour", "seven-day", "seven-day-fable")
	stale := f.handler.authManager.CaptureQuotaObservation(context.Background(), f.auth.ID)
	f.usage = fmt.Sprintf(`{"five_hour":{"utilization":0,"resets_at":%q},"seven_day":{"utilization":30,"resets_at":%q},"limits":[{"kind":"weekly_scoped","percent":0,"is_active":true,"scope":{"model":{"display_name":"Fable"}}}]}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339))
	f.claim(t)
	f.handler.authManager.ApplyQuotaReserveBarsWithContext(stale, f.auth, []coreauth.QuotaReserveBar{{ID: "seven-day", RemainingPercent: 0}})
	if _, err := f.handler.authManager.ApplyProviderQuotaReport(stale, f.auth.ID, true, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.assertSelectable(t, "claude-sonnet-5", true)
	f.assertSelectable(t, "claude-fable-5-2", true)
	got, err := runUsageProbeWindowsWithMode(context.Background(), f.handler, f.auth, "Claude", claudeUsageQuotaURL, parseClaudeGSRQuota, true)
	if err != nil || len(got) == 0 || f.usageCalls.Load() != 2 {
		t.Fatalf("post-reset reconciliation bypassed shared cache or reused old observation: windows=%+v err=%v reads=%d", got, err, f.usageCalls.Load())
	}
}

func TestAPICallClaudePartialResetPreservesProviderReadBackoff(t *testing.T) {
	f := newClaudeResetFixture(t, `["five_hour","seven_day_overage_included"]`)
	f.usageCode = http.StatusTooManyRequests
	f.usage = `{"error":"slow down"}`
	quotaCall(t, f.handler, nil, f.auth.Index, http.MethodGet, claudeUsageQuotaURL, "", "")
	if _, err := f.handler.authManager.ApplyProviderQuotaReport(context.Background(), f.auth.ID, true, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.hold("seven-day", "seven-day-fable")
	f.usageCode = http.StatusOK
	f.usage = `{"five_hour":{"utilization":0},"seven_day":{"utilization":30}}`
	f.claim(t)
	f.assertSelectable(t, "claude-sonnet-5", false)
	response := quotaCall(t, f.handler, nil, f.auth.Index, http.MethodGet, claudeUsageQuotaURL, "", "", true)
	if response.StatusCode != http.StatusTooManyRequests || f.usageCalls.Load() != 1 {
		t.Fatalf("reset bypassed provider backoff: status=%d usage calls=%d", response.StatusCode, f.usageCalls.Load())
	}
}

func TestAPICallClaudeRepeatedResetResponseDoesNotResetNewHolds(t *testing.T) {
	f := newClaudeResetFixture(t, `["five_hour","seven_day","seven_day_overage_included"]`)
	f.claim(t)
	f.blockModel("claude-fable-5-2")
	f.hold("seven-day-fable")
	quotaCall(t, f.handler, nil, f.auth.Index, http.MethodGet, claudeResetGrantsQuotaURL, "", "", true)
	epoch := f.handler.authManager.QuotaObservationEpoch(f.auth.ID)
	f.claim(t)
	f.assertSelectable(t, "claude-fable-5-2", false)
	if f.handler.authManager.QuotaObservationEpoch(f.auth.ID) != epoch {
		t.Fatal("same request_id reset a second time")
	}
}

func TestAPICallClaudeResetReplayAfterReserveEditPreservesNewHolds(t *testing.T) {
	f := newClaudeResetFixture(t, `["five_hour","seven_day","seven_day_overage_included"]`)
	f.claim(t)
	f.blockModel("claude-fable-5-2")
	f.hold("seven-day-fable")
	auth, _ := f.handler.authManager.GetByID(f.auth.ID)
	beforeIdentity := quotaAuthIdentityHash(auth)
	auth.Attributes[coreauth.AttributeQuotaReserve] = `{"five-hour":30,"seven-day":30,"seven-day-fable":40}`
	updated, errUpdate := f.handler.authManager.Update(context.Background(), auth)
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	f.auth = updated
	if quotaAuthIdentityHash(updated) == beforeIdentity {
		t.Fatal("reserve edit did not change the quota cache identity")
	}
	quotaCall(t, f.handler, nil, f.auth.Index, http.MethodGet, claudeResetGrantsQuotaURL, "", "", true)
	epoch := f.handler.authManager.QuotaObservationEpoch(f.auth.ID)
	f.claim(t)
	f.assertSelectable(t, "claude-fable-5-2", false)
	if f.handler.authManager.QuotaObservationEpoch(f.auth.ID) != epoch {
		t.Fatal("reserve edit allowed the same reset request to apply again")
	}
	if state, held := f.handler.authManager.QuotaReserveState(f.auth.ID); !held || len(state.Hits) != 1 || state.Hits[0].BarID != "seven-day-fable" {
		t.Fatalf("replayed reset erased the edited reserve: %+v", state)
	}
}

func TestAPICallClaudeResetReplayOutlivesUsageCache(t *testing.T) {
	f := newClaudeResetFixture(t, `["five_hour","seven_day","seven_day_overage_included"]`)
	f.claim(t)
	f.blockModel("claude-fable-5-2")
	f.hold("seven-day-fable")
	future := time.Now().Add(4 * time.Minute)
	f.handler.quotaCache.now = func() time.Time { return future }
	quotaCall(t, f.handler, nil, f.auth.Index, http.MethodGet, claudeResetGrantsQuotaURL, "", "", true)
	epoch := f.handler.authManager.QuotaObservationEpoch(f.auth.ID)
	f.claim(t)
	f.assertSelectable(t, "claude-fable-5-2", false)
	if f.handler.authManager.QuotaObservationEpoch(f.auth.ID) != epoch {
		t.Fatal("claim replay inside the panel retry window applied another reset")
	}
}

func TestAPICallClaudeResetProjectsFableAliases(t *testing.T) {
	for _, apiKey := range []bool{false, true} {
		for _, full := range []bool{false, true} {
			t.Run(fmt.Sprintf("api_key=%t/full=%t", apiKey, full), func(t *testing.T) {
				clears := `["five_hour","seven_day"]`
				if full {
					clears = `["five_hour","seven_day","seven_day_overage_included"]`
				}
				f := newClaudeResetFixture(t, clears)
				f.auth.Prefix = "team"
				if apiKey {
					f.auth.Attributes["api_key"] = "alias-test-key"
					f.handler.authManager.SetConfig(&config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "alias-test-key", Prefix: "team", Models: []config.ClaudeModel{{Name: "claude-fable-5-2", Alias: "family-only"}}}}})
				} else {
					f.handler.authManager.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{"claude": {{Name: "claude-fable-5-2", Alias: "family-only"}}})
				}
				var err error
				f.auth, err = f.handler.authManager.Update(context.Background(), f.auth)
				if err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(f.auth.ID, "claude", []*registry.ModelInfo{{ID: "team/family-only"}})
				quotaCall(t, f.handler, nil, f.auth.Index, http.MethodGet, claudeResetGrantsQuotaURL, "", "", true)
				f.blockModel("claude-fable-5-2")
				f.hold("seven-day-fable")
				f.assertSelectable(t, "team/family-only(high)", false)
				f.claim(t)
				f.assertSelectable(t, "team/family-only(high)", full)
			})
		}
	}
}

func TestAPICallClaudeResetNeedsProofForUnrefilledSharedWindows(t *testing.T) {
	for _, test := range []struct {
		name      string
		clears    string
		usage     string
		recovered bool
	}{
		{"missing_weekly", `["five_hour","seven_day_overage_included"]`, `{"five_hour":{"utilization":0}}`, false},
		{"known_five_hour_refill", `["five_hour","seven_day_overage_included"]`, `{"seven_day":{"utilization":30}}`, true},
		{"unknown_five_hour", `[]`, `{"seven_day":{"utilization":30}}`, false},
		{"verified_ultra", `[]`, `{"five_hour":null,"seven_day":{"utilization":30}}`, true},
		{"unknown_both_healthy", `[]`, `{"five_hour":{"utilization":0},"seven_day":{"utilization":30}}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newClaudeResetFixture(t, test.clears)
			f.usageCode, f.usage = http.StatusOK, test.usage
			if _, err := f.handler.authManager.ApplyProviderQuotaReport(context.Background(), f.auth.ID, true, time.Now().Add(48*time.Hour)); err != nil {
				t.Fatal(err)
			}
			f.claim(t)
			f.assertSelectable(t, "claude-sonnet-5", test.recovered)
		})
	}
}

func TestAPICallClaudeResetUsesNewestSelectedGrantEvidence(t *testing.T) {
	for _, test := range []struct {
		name    string
		catalog string
		base    bool
	}{
		{"selected_second", `{"cedar_ember":{"grants":[{"id":"other","clears":["five_hour","seven_day","seven_day_overage_included"]},{"id":"selected","clears":["five_hour","seven_day"]}]}}`, true},
		{"missing_new_metadata", `{"cedar_ember":{"grants":[]}}`, false},
		{"malformed_new_metadata", `{"cedar_ember":{"grants":[{"id":"selected","clears":null}]}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newClaudeResetFixture(t, `["five_hour","seven_day","seven_day_overage_included"]`)
			f.catalog = test.catalog
			quotaCall(t, f.handler, nil, f.auth.Index, http.MethodGet, claudeResetGrantsQuotaURL, "newer-header-variant", "", true)
			f.blockModel("claude-fable-5-2")
			if _, err := f.handler.authManager.ApplyProviderQuotaReport(context.Background(), f.auth.ID, true, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			f.hold("seven-day-fable")
			f.claim(t)
			f.assertSelectable(t, "claude-sonnet-5", test.base)
			f.assertSelectable(t, "claude-fable-5-2", false)
		})
	}
}
