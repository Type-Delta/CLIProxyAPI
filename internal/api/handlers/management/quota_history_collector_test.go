package management

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

type testQuotaHistoryService struct{}

func (testQuotaHistoryService) CredentialID(string, string, string) (*string, error) {
	value := "sanitized"
	return &value, nil
}
func (testQuotaHistoryService) RecordQuotaHistory(context.Context, []store.QuotaHistorySample) error {
	return nil
}
func (testQuotaHistoryService) QuotaHistory(context.Context, string, time.Time, time.Time) ([]store.QuotaHistorySample, error) {
	return nil, nil
}

func TestQuotaHistoryCollectorReusesCachedBaseUsage(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	proxy.setResponse(200, nil, `{"plan_type":"pro","rate_limit":{"secondary_window":{"used_percent":35,"limit_window_seconds":604800,"reset_after_seconds":3600}}}`)
	auth := h.authByIndex("credential-a-codex")
	quotaCall(t, h, proxy, "credential-a-codex", "GET", codexUsageQuotaURL, "same", "")
	samples, err := h.collectQuotaHistoryCredential(context.Background(), auth, "codex", testQuotaHistoryService{})
	if err != nil {
		t.Fatal(err)
	}
	if got := proxy.count.Load(); got != 1 {
		t.Fatalf("provider calls=%d, want one cached base request", got)
	}
	if len(samples) != 1 || samples[0].RemainingPercent != 65 || samples[0].PlanID != "pro" || samples[0].Source != "cpa_auto" {
		t.Fatalf("samples=%+v", samples)
	}
}

func TestQuotaHistoryCacheLeaderCancelsOnShutdown(t *testing.T) {
	cache := newAPICallQuotaCache(time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan quotaCallOutcome, 1)
	go func() {
		done <- cache.doBackground(ctx, quotaCacheKey{requestHash: "request", baseHash: "base"}, func(ctx context.Context) quotaCallOutcome { close(started); <-ctx.Done(); return quotaCallOutcome{} })
	}()
	<-started
	cancel()
	result := <-done
	if !result.callerCanceled {
		t.Fatal("canceled collector request was not marked canceled")
	}
	if len(cache.entries) != 0 {
		t.Fatal("canceled collector response was cached")
	}
}

func TestQuotaHistoryCollectorClaudeKeepsBothWeeklyWindows(t *testing.T) {
	proxy := newQuotaProxy(t)
	h := quotaHandler(proxy)
	proxy.setResponse(200, nil, `{"seven_day":{"utilization":20,"resets_at":"2030-01-02T00:00:00Z"},"iguana_necktie":{"utilization":70,"resets_at":"2030-01-03T00:00:00Z"}}`)
	auth := h.authByIndex("credential-a-claude")
	auth.Metadata["plan_type"] = "max"
	samples, err := h.collectQuotaHistoryCredential(context.Background(), auth, "claude", testQuotaHistoryService{})
	if err != nil || len(samples) != 2 || samples[0].WindowID != "seven-day" || samples[0].RemainingPercent != 80 || samples[1].WindowID != "seven-day-fable" || samples[1].RemainingPercent != 30 {
		t.Fatalf("samples=%+v err=%v", samples, err)
	}
	if proxy.count.Load() != 1 {
		t.Fatalf("provider calls=%d", proxy.count.Load())
	}
}
