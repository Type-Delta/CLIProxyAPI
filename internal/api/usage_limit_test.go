package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagelimit"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestUsageLimitPluginAddsNormalizedTokenUsage(t *testing.T) {
	tracker := usagelimit.NewTracker()
	tracker.SetLimits(map[string]usagelimit.Limits{
		"client-key": {MaxTokens: 200},
	})

	(&usageLimitPlugin{tracker: tracker}).HandleUsage(context.Background(), coreusage.Record{
		APIKey:   "client-key",
		Provider: "openai",
		Detail: coreusage.Detail{
			InputTokens:     100,
			OutputTokens:    30,
			ReasoningTokens: 12,
		},
	})

	snapshot := tracker.Snapshot("client-key", time.Now())
	if snapshot == nil || snapshot.TokensUsed != 130 {
		t.Fatalf("token usage = %+v, want 130", snapshot)
	}
}

type usageOrderPlugin struct {
	tracker *usagelimit.Tracker
	mu      sync.Mutex
	seen    int64
}

func (p *usageOrderPlugin) HandleUsage(_ context.Context, _ coreusage.Record) {
	p.mu.Lock()
	defer p.mu.Unlock()
	snapshot := p.tracker.Snapshot("client-key", time.Now())
	if snapshot != nil {
		p.seen = snapshot.TokensUsed
	}
}

func TestUsageLimitAccountingRunsBeforeLossyObservers(t *testing.T) {
	tracker := usagelimit.NewTracker()
	tracker.SetLimits(map[string]usagelimit.Limits{"client-key": {MaxTokens: 200}})
	manager := coreusage.NewManager(1)
	if _, errRegister := manager.RegisterAccountingNamed(usageLimitAccountingName, &usageLimitPlugin{tracker: tracker}); errRegister != nil {
		t.Fatalf("register accounting: %v", errRegister)
	}
	observer := &usageOrderPlugin{tracker: tracker}
	if _, errRegister := manager.RegisterNamed("observer", observer); errRegister != nil {
		t.Fatalf("register observer: %v", errRegister)
	}
	manager.Publish(context.Background(), coreusage.Record{
		APIKey: "client-key", Provider: "openai", Detail: coreusage.Detail{InputTokens: 7, OutputTokens: 5},
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if errClose := manager.Close(ctx); errClose != nil {
		t.Fatalf("close manager: %v", errClose)
	}
	observer.mu.Lock()
	seen := observer.seen
	observer.mu.Unlock()
	if seen != 12 {
		t.Fatalf("observer saw tokens = %d, want synchronous accounting value 12", seen)
	}
}

func TestUsageLimitPluginUsesObservationTime(t *testing.T) {
	tracker := usagelimit.NewTracker()
	tracker.SetLimits(map[string]usagelimit.Limits{
		"client-key": {MaxRequests: 10, MaxTokens: 10_000, Resets: usagelimit.PeriodHourly},
	})

	requestedAt := time.Now().Add(-2 * time.Hour)
	if decision := tracker.Allow("client-key", requestedAt); !decision.Allowed {
		t.Fatalf("old request denied: %+v", decision)
	}

	(&usageLimitPlugin{tracker: tracker}).HandleUsage(context.Background(), coreusage.Record{
		APIKey:      "client-key",
		Provider:    "openai",
		RequestedAt: requestedAt,
		Detail:      coreusage.Detail{InputTokens: 10, OutputTokens: 5},
	})

	snapshot := tracker.Snapshot("client-key", time.Now())
	if snapshot == nil || snapshot.TokensUsed != 15 {
		t.Fatalf("stale feedback snapshot = %+v, want tokens in the active window", snapshot)
	}
}

func TestUsageLimitFromConfig(t *testing.T) {
	tests := []struct {
		name       string
		limits     config.KeyLimits
		wantTokens int64
		wantPeriod usagelimit.Period
	}{
		{name: "omitted resets is lifetime", limits: config.KeyLimits{MaxRequests: 1}, wantPeriod: usagelimit.PeriodLifetime},
		{name: "hourly", limits: config.KeyLimits{Resets: "hourly"}, wantPeriod: usagelimit.PeriodHourly},
		{name: "daily", limits: config.KeyLimits{Resets: "daily"}, wantPeriod: usagelimit.PeriodDaily},
		{name: "weekly", limits: config.KeyLimits{Resets: "weekly"}, wantPeriod: usagelimit.PeriodWeekly},
		{name: "monthly", limits: config.KeyLimits{Resets: "monthly"}, wantPeriod: usagelimit.PeriodMonthly},
		{name: "twenty million tokens", limits: config.KeyLimits{MaxTokensM: 20}, wantTokens: 20_000_000, wantPeriod: usagelimit.PeriodLifetime},
		{name: "half million tokens", limits: config.KeyLimits{MaxTokensM: 0.5}, wantTokens: 500_000, wantPeriod: usagelimit.PeriodLifetime},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, errConvert := usageLimitFromConfig(test.limits)
			if errConvert != nil {
				t.Fatalf("usageLimitFromConfig() error = %v", errConvert)
			}
			if got.MaxTokens != test.wantTokens || got.Resets != test.wantPeriod {
				t.Fatalf("usageLimitFromConfig() = %+v, want tokens=%d resets=%q", got, test.wantTokens, test.wantPeriod)
			}
		})
	}
}
