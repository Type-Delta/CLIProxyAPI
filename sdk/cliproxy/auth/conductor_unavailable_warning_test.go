package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

type quotaLimitTestError struct{}

func (quotaLimitTestError) Error() string {
	return `{"type":"error","error":{"type":"GoUsageLimitError","message":"Weekly usage limit reached."}}`
}

func (quotaLimitTestError) StatusCode() int { return http.StatusTooManyRequests }

func (quotaLimitTestError) RetryAfter() *time.Duration {
	cooldown := 91*time.Hour + 48*time.Minute
	return &cooldown
}

type upstreamServerError struct{}

func (upstreamServerError) Error() string { return `{"error":{"message":"Internal server error"}}` }

func (upstreamServerError) StatusCode() int { return http.StatusInternalServerError }

// TestWarnLogAuthUnavailableReportsErrorBackoffCandidates locks in the
// production failure signature where one credential sits in a long quota
// cooldown while its sibling recovers from upstream 500s in short error
// backoff. The unavailable warning must describe every blocked candidate, not
// only the quota-cooled one, or the log wrongly suggests failover never ran.
func TestWarnLogAuthUnavailableReportsErrorBackoffCandidates(t *testing.T) {
	hook := setupTestLoggerHook(t)
	withQuotaCooldownEnabled(t)

	const (
		provider   = "openai-compatible-opencode go"
		model      = "deepseek-v4.1-flash"
		quotaKeyID = "opencode-go-quota"
		errorKeyID = "opencode-go-erroring"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(3, 0, 0)
	reg := registry.GetGlobalRegistry()
	for _, id := range []string{quotaKeyID, errorKeyID} {
		reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, errRegister := manager.Register(context.Background(), &Auth{
			ID:       id,
			Provider: provider,
			Status:   StatusActive,
		}); errRegister != nil {
			t.Fatalf("register %s: %v", id, errRegister)
		}
	}

	manager.RegisterExecutor(&mixedProviderProbeExecutor{
		identifier: provider,
		execute: func(auth *Auth, _ cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
			if auth.ID == quotaKeyID {
				return cliproxyexecutor.Response{}, quotaLimitTestError{}
			}
			return cliproxyexecutor.Response{}, upstreamServerError{}
		},
	})

	// First request burns both credentials: the quota key into a long cooldown
	// and the erroring key into a short upstream-failure backoff.
	_, _ = manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})

	// The follow-up request finds every candidate blocked and emits the warning
	// under test.
	_, _ = manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})

	var unavailableWarnings []string
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel && strings.Contains(entry.Message, "auth unavailable") {
			unavailableWarnings = append(unavailableWarnings, entry.Message)
		}
	}
	if len(unavailableWarnings) == 0 {
		t.Fatalf("expected auth unavailable warning, got logs: %#v", hook.AllEntries())
	}
	combined := strings.Join(unavailableWarnings, "\n")
	if !strings.Contains(combined, quotaKeyID) {
		t.Fatalf("warning does not mention the quota-cooled credential: %s", combined)
	}
	if !strings.Contains(combined, errorKeyID) {
		t.Fatalf("warning does not mention the error-backoff credential: %s", combined)
	}
	if !strings.Contains(combined, "Internal server error") {
		t.Fatalf("warning does not explain the error-backoff credential: %s", combined)
	}
}
