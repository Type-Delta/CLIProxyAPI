package api

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagelimit"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// usageLimitSnapshotPath returns where usage counters are persisted for authDir.
//
// The snapshot lives in a "state" subdirectory rather than directly in authDir:
// every *.json placed there is treated as credential material, so the watcher
// fires an auth reload for it (internal/watcher/events.go, isAuthJSON) and the
// auth loader picks it up via ReadDir. Both are non-recursive, so nesting keeps
// the snapshot out of that namespace. Do not flatten this path.
func usageLimitSnapshotPath(authDir string) string {
	return filepath.Join(authDir, "state", "usage-limits.json")
}

func usageLimitFromConfig(limits config.KeyLimits) (usagelimit.Limits, error) {
	resets, errParse := usagelimit.ParsePeriod(limits.Resets)
	if errParse != nil {
		return usagelimit.Limits{}, fmt.Errorf("parse usage limit reset period: %w", errParse)
	}
	return usagelimit.Limits{
		MaxRequests: limits.MaxRequests,
		MaxTokens:   limits.MaxTokens(),
		Resets:      resets,
	}, nil
}

func usageLimitsFromConfig(limits map[string]config.KeyLimits) map[string]usagelimit.Limits {
	converted := make(map[string]usagelimit.Limits, len(limits))
	for key, limit := range limits {
		convertedLimit, errConvert := usageLimitFromConfig(limit)
		if errConvert != nil {
			log.WithError(errConvert).Warn("skip API key usage limit with invalid reset period")
			continue
		}
		converted[key] = convertedLimit
	}
	return converted
}

func (s *Server) startUsageLimitPersistence() {
	if s == nil || s.usageLimitTracker == nil || s.usageLimitPath == "" {
		return
	}

	s.usageLimitStop = make(chan struct{})
	s.usageLimitDone = make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		defer close(s.usageLimitDone)
		for {
			select {
			case <-ticker.C:
				s.flushUsageLimits()
			case <-s.usageLimitStop:
				return
			}
		}
	}()
}

func (s *Server) stopUsageLimitPersistence() {
	if s == nil || s.usageLimitStop == nil || s.usageLimitDone == nil {
		return
	}
	s.usageLimitStopOnce.Do(func() { close(s.usageLimitStop) })
	<-s.usageLimitDone
}

func (s *Server) flushUsageLimits() {
	if s == nil || s.usageLimitTracker == nil || s.usageLimitPath == "" {
		return
	}
	if errFlush := s.usageLimitTracker.Flush(s.usageLimitPath); errFlush != nil {
		log.WithError(errFlush).Warn("failed to flush usage limit snapshot")
	}
}

type usageLimitPlugin struct {
	tracker *usagelimit.Tracker
}

const usageLimitAccountingName = "api-key-usage-limits"

// registerUsageLimitAccounting installs token accounting on the trusted inline
// path. The server integration must use this instead of generic observer registration.
func registerUsageLimitAccounting(tracker *usagelimit.Tracker) (coreusage.UnregisterFunc, error) {
	return coreusage.RegisterAccountingNamedPlugin(usageLimitAccountingName, &usageLimitPlugin{tracker: tracker})
}

func (p *usageLimitPlugin) HandleUsage(_ context.Context, record coreusage.Record) {
	if p == nil || p.tracker == nil {
		return
	}

	// Token spend is attributed to the window in which it is observed, not the
	// window the request started in. Usage feedback arrives after the response
	// completes, so backdating it to record.RequestedAt would file tokens into an
	// already-closed window.
	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	p.tracker.AddTokens(record.APIKey, detail.TokenBreakdown.TotalTokens, time.Now())
}
