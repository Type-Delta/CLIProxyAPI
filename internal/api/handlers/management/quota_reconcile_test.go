package management

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestFreshQuotaFetchReconcilesEveryEntryPoint(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "kiro", "kimi", "xai", "zai", "opencode-go"} {
		for _, path := range []string{"management", "analytics", "background", "reserve"} {
			t.Run(provider+"/"+path, func(t *testing.T) {
				fixture := newGSRProbeFixture(t)
				h, auth := gsrProbeHandler(t, provider, map[string]string{"api_key": "test-key"})
				if provider == "kiro" {
					auth.Attributes["region"] = "eu-west-1"
					auth, _ = h.authManager.Update(context.Background(), auth)
				}
				if provider == "zai" || provider == "opencode-go" {
					auth.Attributes["usage_probe"] = provider
					auth, _ = h.authManager.Update(context.Background(), auth)
				}
				const modelID = "quota-reconcile-test"
				registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: modelID}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				h.authManager.RegisterExecutor(&refreshRecordExecutor{provider: provider})
				h.authManager.MarkResult(context.Background(), coreauth.Result{AuthID: auth.ID, Provider: provider, Model: modelID, CredentialVersion: auth.CredentialVersion, CredentialScope: true, Error: &coreauth.Error{HTTPStatus: 429, Message: "quota exhausted"}})
				before, _ := h.authManager.GetByID(auth.ID)
				if !before.Quota.Exceeded || !before.Unavailable {
					t.Fatal("credential was not blocked before quota fetch")
				}
				var target, body string
				switch provider {
				case "claude":
					target, body = claudeUsageQuotaURL, `{"five_hour":{"utilization":10},"seven_day":{"utilization":20}}`
				case "codex":
					target, body = codexUsageQuotaURL, `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":10}}}`
				case "kiro":
					target, body = kiroUsageLimitsURL(auth), `{"usageBreakdownList":[{"currentUsage":1,"usageLimit":100}]}`
				case "kimi":
					target, body = kimiUsageQuotaURL, `{"usage":{"used":1,"limit":100}}`
				case "xai":
					target, body = xaiWeeklyQuotaURL, `{"config":{"creditUsagePercent":10}}`
				case "zai":
					target, body = zaiUsageQuotaURL, `{"code":200,"success":true,"data":{"limits":[{"remaining":90,"percentage":10}]}}`
				case "opencode-go":
					target, body = opencodeGoUsageQuotaURL, `{"usage":{"weekly":{"percent":10}}}`
				}
				fixture.setResponse(http.StatusOK, body)
				switch path {
				case "management":
					quotaCall(t, h, nil, auth.Index, http.MethodGet, target, "", "")
				case "analytics", "background":
					_, errProbe := runUsageProbeWindowsWithMode(context.Background(), h, auth, provider, target, func([]byte) ([]model.ProviderQuotaWindow, error) { return nil, nil }, path == "background")
					if errProbe != nil {
						t.Fatal(errProbe)
					}
				case "reserve":
					var errProbe error
					if provider == "kimi" || provider == "xai" {
						_, errProbe = h.quotaReserveFetch(context.Background(), auth, target, nil)
					} else {
						_, errProbe = h.QuotaReserveBars(context.Background(), auth)
					}
					if errProbe != nil {
						t.Fatal(errProbe)
					}
				}
				current, _ := h.authManager.GetByID(auth.ID)
				if current.Unavailable || current.Quota.Exceeded || current.Status != coreauth.StatusActive {
					t.Fatalf("fresh healthy report left credential blocked: status %s unavailable %v quota %+v", current.Status, current.Unavailable, current.Quota)
				}
				if _, errExecute := h.authManager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: modelID}, cliproxyexecutor.Options{}); errExecute != nil {
					t.Fatalf("request immediately after refresh: %v", errExecute)
				}
			})
		}
	}
}

func TestClaudeFableQuotaRefreshPreservesOtherModelCooldowns(t *testing.T) {
	fixture := newGSRProbeFixture(t)
	h, auth := gsrProbeHandler(t, "claude", nil)
	models := []*registry.ModelInfo{{ID: "claude-fable-5-1"}, {ID: "claude-sonnet-5"}, {ID: "claude-opus-5"}}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, models)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	h.authManager.RegisterExecutor(&refreshRecordExecutor{provider: "claude"})
	opusRetry := 3 * time.Hour
	h.authManager.MarkResult(context.Background(), coreauth.Result{AuthID: auth.ID, Provider: "claude", Model: "claude-opus-5", RetryAfter: &opusRetry, Error: &coreauth.Error{HTTPStatus: 503, Message: "independent outage"}})
	before, _ := h.authManager.GetByID(auth.ID)
	opusDeadline := before.ModelStates["claude-opus-5"].NextRetryAfter
	reset := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	for _, percent := range []int{100, 20} {
		fixture.setResponse(http.StatusOK, fmt.Sprintf(`{"limits":[{"kind":"weekly_scoped","percent":%d,"is_active":true,"resets_at":%q,"scope":{"model":{"display_name":"Fable"}}}]}`, percent, reset))
		quotaCall(t, h, nil, auth.Index, http.MethodGet, claudeUsageQuotaURL, "", "", true)
		current, _ := h.authManager.GetByID(auth.ID)
		if state := current.ModelStates["claude-opus-5"]; !state.NextRetryAfter.Equal(opusDeadline) || state.LastError.HTTPStatus != 503 {
			t.Fatalf("Fable refresh changed Opus cooldown: %+v", state)
		}
		if _, errExecute := h.authManager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-sonnet-5"}, cliproxyexecutor.Options{}); errExecute != nil {
			t.Fatalf("Sonnet request with Fable utilization %d: %v", percent, errExecute)
		}
		_, errFable := h.authManager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-fable-5-1"}, cliproxyexecutor.Options{})
		if (errFable != nil) != (percent == 100) {
			t.Fatalf("Fable utilization %d: execution error = %v", percent, errFable)
		}
	}
}

func TestClaudeFableQuotaWithNoModelHistoryLeavesOpusUsable(t *testing.T) {
	fixture := newGSRProbeFixture(t)
	h, auth := gsrProbeHandler(t, "claude", nil)
	h.authManager.RegisterExecutor(&refreshRecordExecutor{provider: "claude"})
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "claude-fable-5-1"}, {ID: "claude-opus-5"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	fixture.setResponse(http.StatusOK, fmt.Sprintf(`{"five_hour":{"utilization":1},"seven_day":{"utilization":79},"limits":[{"kind":"weekly_scoped","percent":100,"is_active":true,"resets_at":%q,"scope":{"model":{"display_name":"Fable"}}}]}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)))
	quotaCall(t, h, nil, auth.Index, http.MethodGet, claudeUsageQuotaURL, "", "")
	if _, errExecute := h.authManager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-opus-5"}, cliproxyexecutor.Options{}); errExecute != nil {
		t.Fatalf("Opus blocked by first observed Fable quota: %v", errExecute)
	}
	if _, errExecute := h.authManager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-fable-5-1"}, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("exhausted Fable should remain blocked")
	}
}

func TestQuotaReadBeforeReserveEnabledAppliesImmediately(t *testing.T) {
	fixture := newGSRProbeFixture(t)
	h, auth := gsrProbeHandler(t, "claude", nil)
	fixture.setResponse(http.StatusOK, `{"seven_day":{"utilization":80,"resets_at":"2030-01-01T00:00:00Z"}}`)
	quotaCall(t, h, nil, auth.Index, http.MethodGet, claudeUsageQuotaURL, "", "")
	auth.Metadata = map[string]any{coreauth.AttributeQuotaReserve: map[string]any{"seven-day": 30.0}}
	coreauth.ApplyQuotaReserveMetadata(auth)
	if _, errUpdate := h.authManager.Update(context.Background(), auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if _, held := h.authManager.QuotaReserveState(auth.ID); !held {
		t.Fatal("new reserve ignored the previously fetched quota")
	}
}

func TestAnalyticsCachedQuotaCannotClearNewCooldown(t *testing.T) {
	fixture := newGSRProbeFixture(t)
	h, auth := gsrProbeHandler(t, "openai-compatible-zai", map[string]string{"usage_probe": "zai", "api_key": "test-key"})
	fixture.setResponse(http.StatusOK, `{"code":200,"success":true,"data":{"limits":[{"remaining":90,"percentage":10}]}}`)
	credentials := map[string]*coreauth.Auth{"test": auth}
	h.applyUsageProbes(context.Background(), credentials, map[string]model.ProviderCredential{})
	if _, errApply := h.authManager.ApplyProviderQuotaReport(context.Background(), auth.ID, true, time.Now().Add(time.Hour)); errApply != nil {
		t.Fatal(errApply)
	}
	h.applyUsageProbes(context.Background(), credentials, map[string]model.ProviderCredential{})
	current, _ := h.authManager.GetByID(auth.ID)
	if !current.Unavailable || !current.Quota.Exceeded {
		t.Fatal("analytics cached healthy response cleared a newer quota refusal")
	}
	if got := len(fixture.requests()); got != 1 {
		t.Fatalf("upstream requests %d, want one plus cached replay", got)
	}
}

func TestCodexIndependentAllowanceRecoversWhileStandardExhausted(t *testing.T) {
	fixture := newGSRProbeFixture(t)
	h, auth := gsrProbeHandler(t, "codex", nil)
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-5.5"}, {ID: "gpt-5.3-codex-spark"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	h.authManager.RegisterExecutor(&refreshRecordExecutor{provider: "codex"})
	h.authManager.MarkResult(context.Background(), coreauth.Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5.5", CredentialScope: true, Error: &coreauth.Error{HTTPStatus: 429}})
	fixture.setResponse(http.StatusOK, fmt.Sprintf(`{"rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"reset_at":%d}},"additional_rate_limits":[{"limit_name":"gpt-5.3-codex-spark","rate_limit":{"allowed":true,"primary_window":{"used_percent":10}}}]}`, time.Now().Add(time.Hour).Unix()))
	quotaCall(t, h, nil, auth.Index, http.MethodGet, codexUsageQuotaURL, "", "")
	if _, errExecute := h.authManager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.3-codex-spark"}, cliproxyexecutor.Options{}); errExecute != nil {
		t.Fatalf("healthy Spark allowance blocked: %v", errExecute)
	}
	if _, errExecute := h.authManager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.5"}, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("exhausted standard allowance became selectable")
	}
}

func TestAntigravityQuotaPoolsRemainIndependent(t *testing.T) {
	fixture := newGSRProbeFixture(t)
	h, auth := gsrProbeHandler(t, "antigravity", nil)
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gemini-test"}, {ID: "claude-test"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	h.authManager.RegisterExecutor(&refreshRecordExecutor{provider: "antigravity"})
	fixture.setResponse(http.StatusOK, fmt.Sprintf(`{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","remainingFraction":0,"resetTime":%q}]},{"displayName":"Claude and GPT Models","buckets":[{"bucketId":"claude-weekly","remainingFraction":0.5}]}]}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)))
	quotaCall(t, h, nil, auth.Index, http.MethodPost, "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", "", `{}`)
	if _, errExecute := h.authManager.Execute(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: "claude-test"}, cliproxyexecutor.Options{}); errExecute != nil {
		t.Fatalf("healthy Claude pool blocked: %v", errExecute)
	}
	if _, errExecute := h.authManager.Execute(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: "gemini-test"}, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("exhausted Gemini pool became selectable")
	}
}

func TestXaiPaidHealthProbeRecoversItsAllowance(t *testing.T) {
	fixture := newGSRProbeFixture(t)
	h, auth := gsrProbeHandler(t, "xai", nil)
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: xaiHealthModel}, {ID: "grok-independent"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	h.authManager.RegisterExecutor(&refreshRecordExecutor{provider: "xai"})
	if _, err := h.authManager.ApplyProviderQuotaReportForModels(context.Background(), auth.ID, true, time.Now().Add(3*time.Hour), []string{"grok-independent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.authManager.ApplyProviderQuotaReport(context.Background(), auth.ID, true, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	fixture.setResponse(http.StatusOK, `{"object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	response := quotaCall(t, h, nil, auth.Index, http.MethodPost, xaiHealthURL, "", `{"model":"grok-4.5","messages":[{"role":"user","content":"ping"}],"max_tokens":1,"stream":false}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("paid health response: %+v", response)
	}
	if _, err := h.authManager.Execute(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: xaiHealthModel}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("healthy paid allowance stayed blocked: %v", err)
	}
	if _, err := h.authManager.Execute(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: "grok-independent"}, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("paid health probe erased another model's independent limit")
	}
}

func TestFreshQuotaFetchReconcilesAliases(t *testing.T) {
	for _, tc := range []struct{ provider, upstream, alias string }{
		{"claude", "claude-fable-5-1", "family-only"},
		{"codex", "gpt-5.5", "standard-only"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			fixture := newGSRProbeFixture(t)
			h, auth := gsrProbeHandler(t, tc.provider, nil)
			h.authManager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{tc.provider: {{Name: tc.upstream, Alias: tc.alias}}})
			registry.GetGlobalRegistry().RegisterClient(auth.ID, tc.provider, []*registry.ModelInfo{{ID: tc.alias}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			h.authManager.RegisterExecutor(&refreshRecordExecutor{provider: tc.provider})
			reset := time.Now().Add(time.Hour)
			var target, exhausted, healthy string
			if tc.provider == "claude" {
				target = claudeUsageQuotaURL
				exhausted = fmt.Sprintf(`{"limits":[{"kind":"weekly_scoped","percent":100,"is_active":true,"resets_at":%q,"scope":{"model":{"display_name":"Fable"}}}]}`, reset.UTC().Format(time.RFC3339))
				healthy = `{"limits":[{"kind":"weekly_scoped","percent":20,"is_active":true,"scope":{"model":{"display_name":"Fable"}}}]}`
			} else {
				target = codexUsageQuotaURL
				exhausted = fmt.Sprintf(`{"rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"reset_at":%d}}}`, reset.Unix())
				healthy = `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":10}}}`
			}
			fixture.setResponse(http.StatusOK, exhausted)
			quotaCall(t, h, nil, auth.Index, http.MethodGet, target, "", "", true)
			if _, err := h.authManager.Execute(context.Background(), []string{tc.provider}, cliproxyexecutor.Request{Model: tc.alias}, cliproxyexecutor.Options{}); err == nil {
				t.Fatal("exhausted alias remained selectable")
			}
			fixture.setResponse(http.StatusOK, healthy)
			quotaCall(t, h, nil, auth.Index, http.MethodGet, target, "", "", true)
			if _, err := h.authManager.Execute(context.Background(), []string{tc.provider}, cliproxyexecutor.Request{Model: tc.alias}, cliproxyexecutor.Options{}); err != nil {
				t.Fatalf("healthy alias remained blocked: %v", err)
			}
		})
	}
}

func TestCodexManualResetRecoversAliasedStandardModel(t *testing.T) {
	fixture := newGSRProbeFixture(t)
	h, auth := gsrProbeHandler(t, "codex", nil)
	h.authManager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"codex": {{Name: "gpt-5.5", Alias: "standard-only"}}})
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "standard-only"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	h.authManager.RegisterExecutor(&refreshRecordExecutor{provider: "codex"})
	retry := time.Hour
	h.authManager.MarkResult(context.Background(), coreauth.Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5.5", RetryAfter: &retry, Error: &coreauth.Error{HTTPStatus: 429}})
	fixture.setResponse(http.StatusOK, `{}`)
	quotaCall(t, h, nil, auth.Index, http.MethodPost, codexConsumeURL, "", `{}`)
	if _, err := h.authManager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "standard-only"}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("alias blocked after reset: %v", err)
	}
}

func TestCodexAliasedIndependentAllowanceSurvivesStandardReset(t *testing.T) {
	fixture := newGSRProbeFixture(t)
	h, auth := gsrProbeHandler(t, "codex", nil)
	h.authManager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"codex": {
		{Name: "gpt-5.5", Alias: "standard-only"},
		{Name: "gpt-5.3-codex-spark", Alias: "fast-only"},
	}})
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "standard-only"}, {ID: "fast-only"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	h.authManager.RegisterExecutor(&refreshRecordExecutor{provider: "codex"})
	reset := time.Now().Add(time.Hour).Unix()
	fixture.setResponse(http.StatusOK, fmt.Sprintf(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":10}},"additional_rate_limits":[{"limit_name":"gpt-5.3-codex-spark","rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"reset_at":%d}}}]}`, reset))
	quotaCall(t, h, nil, auth.Index, http.MethodGet, codexUsageQuotaURL, "", "")
	if _, err := h.authManager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "fast-only"}, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("exhausted Spark alias remained selectable")
	}
	fixture.setResponse(http.StatusOK, `{}`)
	quotaCall(t, h, nil, auth.Index, http.MethodPost, codexConsumeURL, "", `{}`)
	if _, err := h.authManager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "standard-only"}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("standard alias blocked: %v", err)
	}
	if _, err := h.authManager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "fast-only"}, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("manual standard reset cleared Spark alias")
	}
}
