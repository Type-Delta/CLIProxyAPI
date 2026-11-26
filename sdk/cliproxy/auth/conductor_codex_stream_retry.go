package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const codexStreamRetryMinDelay = time.Second

func canRetryCodexStream(ctx context.Context, req cliproxyexecutor.Request) bool {
	return ctx != nil && ctx.Done() != nil && !cliproxyexecutor.RequiredUpstreamWebsocket(ctx) &&
		strings.TrimSpace(ojson.GetBytesNoCopy(req.Payload, "previous_response_id").String()) == ""
}

func (m *Manager) codexStreamRetryDelay(ctx context.Context, opts cliproxyexecutor.Options, providers []string, model string, err error, attempted map[string]struct{}) (time.Duration, bool) {
	if m.HomeEnabled() || !cliproxyexecutor.CodexStreamRetryEnabled(ctx) || ctx.Err() != nil ||
		isRequestInvalidError(err) || isTerminalRequestStopError(err) || isCredentialTerminatedError(err) || cliproxyexecutor.IsUpstreamWebsocketTurnError(err) ||
		isSessionAffinityRetryError(err) || hasRoundEndedBySchedulerReject(err) {
		return 0, false
	}
	if !hasUpstreamExecutionAttempt(err) {
		var cooldown *modelCooldownError
		if errors.As(err, &cooldown) &&
			(canonicalSchedulingProvider(cooldown.provider) == "codex" || len(providers) == 1 && providersIncludeCodex(providers)) {
			cause := resultErrorFromError(cooldown.cause)
			if isTransientRateLimitResultError(cause) || isServerOverloadedResultError(cause) {
				return max(codexStreamRetryMinDelay, cooldown.resetIn), true
			}
		}
		return 0, false
	}
	if !m.isCodexAttemptFailure(err, attempted) {
		return 0, false
	}
	var authErr *Error
	emptyStream := errors.As(err, &authErr) && authErr.Code == "empty_stream"
	if !emptyStream && !isConnectionLifecycleError(err) && !isTransientTransportError(err) &&
		!isTransientRateLimitError(err) && !isServerOverloadedResultError(resultErrorFromError(err)) {
		return 0, false
	}
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	if affinity, ok := m.Selector().(*SessionAffinitySelector); ok && affinity != nil && affinity.rebindOnNever && pinnedAuthID == "" {
		pinnedAuthID, _ = affinity.boundAuthForRequest(providers, model, opts)
	}
	// A recovery sweep starts at round zero, so include credentials with
	// an explicit zero ordinary retry budget when computing availability.
	wait, found := m.closestCooldownWaitWithAttemptedForRequest(ctx, providers, model, -1,
		m.eligibilityForRequest(ctx, opts, model), pinnedAuthID, 0, 0, nil, opts.Headers)
	if !found {
		return 0, false
	}
	if wait > 0 {
		return max(codexStreamRetryMinDelay, wait), true
	}

	if hint := retryAfterFromError(err); hint != nil && *hint > 0 {
		return max(codexStreamRetryMinDelay, *hint), true
	}
	return codexStreamRetryMinDelay, true
}
