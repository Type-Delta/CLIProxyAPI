package auth

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
)

type routingCaptureKey struct{}
type routingRetryDefaultKey struct{}
type routingRetryRoundKey struct{}
type routingCapture struct {
	trace   *routing.Trace
	indexes map[string]int
}

func (m *Manager) beginRoutingCapture(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (context.Context, *routingCapture) {
	holder := routing.HolderFromContext(ctx)
	if holder == nil {
		return ctx, nil
	}
	if _, ok := ctx.Value(routingCaptureKey{}).(*routingCapture); ok {
		return ctx, nil
	}
	trace := &routing.Trace{Version: routing.TraceVersion, Mode: routing.ModeLocal, Pick: -1, DecidedBy: routing.DecidedByNone, Affinity: &routing.AffinityStep{State: routing.AffinityOff, Bound: -1}, RetryRound: homeRetryRoundFromMetadata(opts.Metadata)}
	trace.Candidates = make([]routing.Candidate, 0)
	if round, ok := ctx.Value(routingRetryRoundKey{}).(int); ok {
		trace.RetryRound = round
	}
	if pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		trace.DecidedBy = routing.DecidedByPin
	}
	if trace.RetryRound < 0 {
		trace.RetryRound = 0
	}
	if last := holder.Last(); last != nil {
		trace.Selection = last.Selection + 1
	}
	capture := &routingCapture{trace: trace, indexes: make(map[string]int)}
	if m.HomeEnabled() {
		trace.Mode = routing.ModeHome
		trace.DecidedBy = routing.DecidedByHome
		trace.Affinity = nil
		return context.WithValue(ctx, routingCaptureKey{}, capture), capture
	}
	providerSet := make(map[string]bool, len(providers))
	for _, p := range providers {
		providerSet[canonicalSchedulingProvider(p)] = true
	}
	m.mu.RLock()
	selector := m.selector
	if affinity, ok := selector.(*SessionAffinitySelector); ok {
		trace.Affinity.State = routing.AffinityNone
		selector = affinity.fallback
	}
	weighted := false
	switch selector.(type) {
	case *RoundRobinSelector:
		trace.Strategy = "round-robin"
	case *WeightedRoundRobinSelector:
		trace.Strategy = "weighted-round-robin"
		weighted = true
	case *FillFirstSelector:
		trace.Strategy = "fill-first"
	case *GreedySoonestRecoverySelector:
		trace.Strategy = "greedy-soonest-recovery"
	default:
		trace.Strategy = "custom"
	}
	eligibility := m.eligibilityForRequest(ctx, opts)
	defaultRetry := m.retrySettingsDefault()
	if snapshot, ok := ctx.Value(routingRetryDefaultKey{}).(int); ok {
		defaultRetry = snapshot
	}
	pin := pinnedAuthIDFromMetadata(opts.Metadata)
	now := time.Now()
	var auths []*Auth
	for _, a := range m.auths {
		if a != nil && providerSet[canonicalSchedulingProvider(executorKeyFromAuth(a))] {
			auths = append(auths, a)
		}
	}
	sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
	reg := registry.GetGlobalRegistry()
	for _, a := range auths {
		clone := a.Clone()
		c := routing.Candidate{ID: a.ID, Provider: a.Provider, Priority: authPriority(a), AuthIndex: clone.EnsureIndex()}
		blocked, reason, until := isAuthBlockedForModel(a, m.selectionModelForAuth(a, model), now)
		expiresAt, hasExpiry := a.AccessTokenExpirationTime()
		expired := hasExpiry && !expiresAt.IsZero() && !expiresAt.After(now)
		switch {
		case a.Disabled || reason == blockReasonDisabled:
			c.Gate = routing.GateDisabled
		case pin != "" && a.ID != pin:
			c.Gate = routing.GatePinned
		case !eligibility.allows(a):
			c.Gate = routing.GatePolicy
		case strings.TrimSpace(model) != "" && !m.authSupportsRouteModel(reg, a, model):
			c.Gate = routing.GateUnsupported
		case hasUnauthorizedAuthFailure(a) || expired:
			c.Gate = routing.GateUnauthorized
		case blocked && (reason == blockReasonCooldown || !until.IsZero()):
			c.Gate = routing.GateCooldown
			if a.Quota.Exceeded && a.Quota.Reason == "credential_quota" {
				c.Gate = routing.GateQuota
			}
			if !until.IsZero() {
				c.UntilMs = until.UnixMilli()
			}
		case blocked:
			c.Gate = routing.GatePolicy
		case weighted && authWeight(a) <= 0:
			c.Gate = routing.GateWeight
		case trace.RetryRound > 0 && effectiveRequestRetryLimit(a, defaultRetry) < trace.RetryRound:
			c.Gate = routing.GateRetryLimit
		default:
			if _, ok := tried[a.ID]; ok {
				c.Gate = routing.GateTried
			}
		}
		capture.indexes[c.ID] = len(trace.Candidates)
		trace.Candidates = append(trace.Candidates, c)
	}
	m.mu.RUnlock()
	return context.WithValue(ctx, routingCaptureKey{}, capture), capture
}

func (m *Manager) retrySettingsDefault() int { retry, _, _ := m.retrySettings(); return retry }

func finishRoutingCapture(ctx context.Context, capture *routingCapture, selected *Auth, err error) {
	if capture == nil {
		return
	}
	t := capture.trace
	if selected != nil {
		i, ok := capture.indexes[selected.ID]
		if !ok {
			i = len(t.Candidates)
			clone := selected.Clone()
			t.Candidates = append(t.Candidates, routing.Candidate{ID: selected.ID, Provider: selected.Provider, Priority: authPriority(selected), AuthIndex: clone.EnsureIndex()})
			capture.indexes[selected.ID] = i
		}
		t.Pick = i
		if t.DecidedBy == routing.DecidedByNone {
			t.DecidedBy = routing.DecidedByStrategy
		}
	} else {
		eligible := false
		for _, candidate := range t.Candidates {
			if candidate.Gate == "" {
				eligible = true
				break
			}
		}
		if !eligible {
			t.Error = "no_eligible"
		}
	}
	if t.Tier == nil && t.Mode != routing.ModeHome && t.DecidedBy != routing.DecidedByAffinity {
		best := 0
		have := false
		for i, c := range t.Candidates {
			if c.Gate != "" {
				continue
			}
			if !have || c.Priority > best {
				best = c.Priority
				t.Tier = &routing.TierStep{Priority: best, Pool: []int{i}}
				have = true
			} else if c.Priority == best {
				t.Tier.Pool = append(t.Tier.Pool, i)
			}
		}
	}
	routing.HolderFromContext(ctx).Set(t)
}

func captureRoutingDecision(ctx context.Context, decision routing.Decision, auths []*Auth) {
	c, _ := ctx.Value(routingCaptureKey{}).(*routingCapture)
	if c == nil {
		return
	}
	step := &routing.RulesStep{Applied: decision.Applied, Matched: make([]routing.RuleTrace, 0), Pool: make([]int, 0)}
	for _, match := range decision.Matched {
		r := routing.RuleTrace{Index: match.Index, Name: match.Name, Condition: match.Condition, Status: match.Status, Targets: make([]int, 0)}
		for _, id := range match.Targets {
			if i, ok := c.indexes[id]; ok {
				r.Targets = append(r.Targets, i)
			}
		}
		step.Matched = append(step.Matched, r)
	}
	if decision.Allowed != nil {
		for _, a := range auths {
			if i, ok := c.indexes[a.ID]; ok {
				step.Pool = append(step.Pool, i)
			}
		}
	}
	c.trace.Rules = step
	tier := highestPriorityAuths(auths)
	if len(tier) > 0 {
		stepTier := &routing.TierStep{Priority: authPriority(tier[0]), Pool: make([]int, 0)}
		for _, a := range tier {
			if i, ok := c.indexes[a.ID]; ok {
				stepTier.Pool = append(stepTier.Pool, i)
			}
		}
		c.trace.Tier = stepTier
	}
}

// captureRevalidatedGate updates a candidate after a quota refresh; m.mu must be held.
func captureRevalidatedGate(ctx context.Context, id string, auth *Auth, model string, m *Manager, allow func(*Auth) bool) {
	c, _ := ctx.Value(routingCaptureKey{}).(*routingCapture)
	if c == nil {
		return
	}
	index, ok := c.indexes[id]
	if !ok {
		return
	}
	candidate := &c.trace.Candidates[index]
	if candidate.Gate != "" {
		return
	}
	if auth == nil {
		candidate.Gate = routing.GateGone
		return
	}
	now := time.Now()
	blocked, reason, until := isAuthBlockedForModel(auth, m.selectionModelForAuth(auth, model), now)
	expiresAt, hasExpiry := auth.AccessTokenExpirationTime()
	expired := hasExpiry && !expiresAt.IsZero() && !expiresAt.After(now)
	switch {
	case auth.Disabled || reason == blockReasonDisabled:
		candidate.Gate = routing.GateDisabled
	case strings.TrimSpace(model) != "" && !m.authSupportsRouteModel(registry.GetGlobalRegistry(), auth, model):
		candidate.Gate = routing.GateUnsupported
	case !allow(auth):
		candidate.Gate = routing.GatePolicy
	case hasUnauthorizedAuthFailure(auth) || expired:
		candidate.Gate = routing.GateUnauthorized
	case blocked && (reason == blockReasonCooldown || !until.IsZero()):
		candidate.Gate = routing.GateCooldown
		if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" {
			candidate.Gate = routing.GateQuota
		}
		if !until.IsZero() {
			candidate.UntilMs = until.UnixMilli()
		}
	case blocked:
		candidate.Gate = routing.GatePolicy
	}
}

func captureRevalidatedUniverse(ctx context.Context, m *Manager, model string) {
	c, _ := ctx.Value(routingCaptureKey{}).(*routingCapture)
	if c == nil {
		return
	}
	for _, candidate := range c.trace.Candidates {
		captureRevalidatedGate(ctx, candidate.ID, m.auths[candidate.ID], model, m, func(*Auth) bool { return true })
	}
}

func captureRoutingTier(ctx context.Context, auths []*Auth) {
	c, _ := ctx.Value(routingCaptureKey{}).(*routingCapture)
	if c == nil {
		return
	}
	c.trace.Tier = nil
	if len(auths) == 0 {
		return
	}
	tier := &routing.TierStep{Priority: authPriority(auths[0]), Pool: make([]int, 0)}
	for _, auth := range auths {
		if i, ok := c.indexes[auth.ID]; ok {
			tier.Pool = append(tier.Pool, i)
		}
	}
	c.trace.Tier = tier
}

// captureAffinityRules records how matching rules relate to an eligible affinity binding.
func captureAffinityRules(ctx context.Context, eligible []*Auth) {
	capture, _ := ctx.Value(routingCaptureKey{}).(*routingCapture)
	if capture == nil {
		return
	}
	if pool, _ := ctx.Value(routingPoolKey{}).(*routingPool); pool == nil {
		return
	}
	previousTier := capture.trace.Tier
	narrowRoutingPool(ctx, eligible)
	capture.trace.Tier = previousTier
	if rules := capture.trace.Rules; rules != nil {
		for _, index := range rules.Pool {
			if index == capture.trace.Affinity.Bound {
				return
			}
		}
		for i := range rules.Matched {
			if rules.Matched[i].Status == routing.RuleApplied {
				rules.Matched[i].Status = routing.RuleOverridden
			}
		}
		rules.Applied = -1
		rules.Pool = nil
	}
}

func captureAffinity(ctx context.Context, state, bound string) {
	c, _ := ctx.Value(routingCaptureKey{}).(*routingCapture)
	if c == nil {
		return
	}
	c.trace.Affinity.State = state
	if bound != "" {
		i, ok := c.indexes[bound]
		if !ok {
			i = len(c.trace.Candidates)
			c.indexes[bound] = i
			c.trace.Candidates = append(c.trace.Candidates, routing.Candidate{ID: bound, Gate: routing.GateGone})
		}
		c.trace.Affinity.Bound = i
	}
	if state == routing.AffinityKept && c.trace.DecidedBy != routing.DecidedByPin {
		c.trace.DecidedBy = routing.DecidedByAffinity
	}
}

func markRoutingDecidedBy(ctx context.Context, by string) {
	if c, ok := ctx.Value(routingCaptureKey{}).(*routingCapture); ok {
		if c.trace.DecidedBy != routing.DecidedByPin && c.trace.DecidedBy != routing.DecidedByAffinity {
			c.trace.DecidedBy = by
		}
	}
}

func recordExceptionalRouting(ctx context.Context, mode, decidedBy string, auth *Auth, retryRound int, err error) context.Context {
	holder := routing.HolderFromContext(ctx)
	if holder == nil {
		return ctx
	}
	trace := &routing.Trace{Version: routing.TraceVersion, Mode: mode, DecidedBy: decidedBy, Pick: -1, RetryRound: retryRound, Candidates: make([]routing.Candidate, 0)}
	if previous := holder.Last(); previous != nil {
		trace.Selection = previous.Selection + 1
		if decidedBy == routing.DecidedByCredits {
			trace.RetryRound = previous.RetryRound
		}
	}
	if auth != nil {
		clone := auth.Clone()
		trace.Candidates = []routing.Candidate{{ID: auth.ID, Provider: auth.Provider, Priority: authPriority(auth), AuthIndex: clone.EnsureIndex()}}
		trace.Pick = 0
	} else if err != nil {
		trace.Error = "no_eligible"
	}
	holder.Set(trace)
	return routing.WithTrace(ctx, trace)
}
