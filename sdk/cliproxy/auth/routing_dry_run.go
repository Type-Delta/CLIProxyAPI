package auth

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

// DryRunRoute evaluates a hypothetical request without advancing routing state.
func (m *Manager) DryRunRoute(ctx context.Context, req routing.DryRunRequest) (*routing.Trace, error) {
	if m.HomeEnabled() {
		return &routing.Trace{Version: routing.TraceVersion, Mode: routing.ModeDryRun, DecidedBy: routing.DecidedByHome, Pick: -1, Candidates: []routing.Candidate{}}, nil
	}
	model := strings.TrimSpace(req.Model)
	initial := thinking.ParseSuffix(model)
	if strings.EqualFold(initial.ModelName, "auto") {
		model = util.ResolveAutoModel(initial.ModelName)
		if initial.HasSuffix {
			model += "(" + initial.RawSuffix + ")"
		}
	} else {
		model = util.ResolveAutoModel(model)
	}
	parsed := thinking.ParseSuffix(model)
	providers := util.GetProviderName(strings.TrimSpace(parsed.ModelName))
	if len(providers) == 0 && parsed.ModelName != model {
		providers = util.GetProviderName(model)
	}
	providers = m.normalizeProviders(providers)
	headers := req.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	metadata := map[string]any{cliproxyexecutor.RequestedModelMetadataKey: req.Model, cliproxyexecutor.ClientKeyIDMetadataKey: req.KeyID}
	if cfg, ok := m.runtimeConfig.Load().(*internalconfig.Config); ok && cfg != nil && req.KeyID != "" {
		for _, key := range cfg.APIKeys {
			if internalconfig.APIKeyID(key.Key) == req.KeyID {
				metadata[cliproxyexecutor.CallerScopeMetadataKey] = cliproxysession.CallerScope(key.Key)
				break
			}
		}
	}
	primaryID, fallbackID := extractExplicitSessionIDs(headers, nil, metadata)
	if primaryID == "" && strings.TrimSpace(req.SessionID) != "" {
		headers.Set("X-Session-ID", req.SessionID)
		primaryID, fallbackID = extractExplicitSessionIDs(headers, nil, metadata)
	}
	opts := cliproxyexecutor.Options{Headers: headers, Metadata: metadata}
	ctx, _ = routing.WithTraceHolder(ctx)
	ctx, capture := m.beginRoutingCapture(ctx, providers, model, opts, nil)
	trace := capture.trace
	trace.Mode = routing.ModeDryRun
	m.mu.RLock()
	selector := m.selector
	strategyModel := selectionArgForSelector(selector, model)
	eligible := make([]*Auth, 0, len(trace.Candidates))
	var affinityCandidates []*Auth
	affinity, affinityEnabled := selector.(*SessionAffinitySelector)
	eligibility := m.eligibilityForRequest(ctx, opts)
	for _, c := range trace.Candidates {
		a := m.auths[c.ID]
		if a == nil {
			continue
		}
		if c.Gate == "" {
			eligible = append(eligible, a.Clone())
		}
		if affinityEnabled && affinity.rebindOnQuotaOnly && !a.Disabled && eligibility.allows(a) &&
			(strings.TrimSpace(model) == "" || m.authSupportsRouteModel(registry.GetGlobalRegistry(), a, model)) {
			if _, ok := m.executorLocked(executorKeyFromAuth(a)); ok {
				affinityCandidates = append(affinityCandidates, a.Clone())
			}
		}
	}
	m.mu.RUnlock()
	provider := "mixed"
	if len(providers) == 1 {
		provider = providers[0]
	}
	rules := req.Rules
	if rules == nil {
		rules = m.compiledRoutingRules()
	}
	if rules != nil && rules.Len() > 0 {
		clientType := req.ClientType
		if clientType == "" {
			if info, ok := cliproxysession.ExtractSessionInfo(headers, nil, metadata); ok {
				clientType = info.ClientType
			}
		}
		attrs := routing.RequestAttrs{Model: req.Model, KeyID: req.KeyID, ClientType: clientType, Headers: headers}
		ctx = context.WithValue(ctx, routingPoolKey{}, &routingPool{decide: rules.Decide, attrs: attrs})
	}
	var heldBinding string
	if affinityEnabled {
		selector = affinity.fallback
		preserveCtx := selectorContextForAvailableAuths(ctx, affinity, model)
		transientIDs := m.transientlyUnavailableSessionAffinityAuthIDs(affinity, affinityCandidates, model, time.Now())
		preserveCtx = withSessionAffinityTransientAuthIDs(preserveCtx, transientIDs)
		if primaryID != "" {
			trace.Affinity.State = routing.AffinityNew
			primaryID = cliproxysession.BoundSessionIdentity(primaryID)
			fallbackID = cliproxysession.BoundSessionIdentity(fallbackID)
			identities := []string{primaryID}
			if fallbackID != "" && fallbackID != primaryID {
				identities = append(identities, fallbackID)
			}
			isFork, _ := metadata[cliproxyexecutor.IsForkMetadataKey].(bool)
			isSubagent := !isFork && isSubagentSession(primaryID, fallbackID)
			for _, identity := range identities {
				if identity != primaryID && isSubagent && !affinity.subagentAffinity {
					continue
				}
				key := provider + "::" + identity + "::" + canonicalModelKey(selectionArgForSelector(affinity, model))
				affinity.cache.mu.RLock()
				entry, found := affinity.cache.entries[key]
				affinity.cache.mu.RUnlock()
				if !found || !time.Now().Before(entry.expiresAt) {
					continue
				}
				captureAffinity(ctx, routing.AffinityBroken, entry.authID)
				for _, a := range eligible {
					if a.ID == entry.authID {
						captureAffinity(ctx, routing.AffinityKept, a.ID)
						captureAffinityRules(ctx, eligible)
						trace.Pick = capture.indexes[a.ID]
						trace.PickPredicted = true
						return trace, nil
					}
				}
				if (identity == primaryID || (!isSubagent && !isFork)) && affinity.shouldPreserveBindingWhenUnavailable(preserveCtx, affinityCandidates, model, entry.authID, time.Now()) {
					heldBinding = entry.authID
					break
				}
				// Real selection reselects immediately when the primary binding is blocked.
				if identity == primaryID {
					break
				}
			}
		}
	}
	if rules != nil && rules.Len() > 0 {
		eligible = narrowRoutingPool(ctx, eligible)
	}

	tier := highestPriorityAuths(eligible)
	if len(tier) == 0 {
		trace.Error = "no_eligible"
		return trace, nil
	}
	if heldBinding != "" {
		captureAffinity(ctx, routing.AffinityHeld, heldBinding)
	}
	trace.Tier = &routing.TierStep{Priority: authPriority(tier[0])}
	for _, a := range tier {
		trace.Tier.Pool = append(trace.Tier.Pool, capture.indexes[a.ID])
	}
	sort.Slice(tier, func(i, j int) bool { return tier[i].ID < tier[j].ID })
	trace.DecidedBy = routing.DecidedByStrategy
	if m.hasPluginScheduler() {
		return trace, nil
	}
	var chosen *Auth
	switch s := selector.(type) {
	case *FillFirstSelector:
		chosen = tier[0]
		if len(providers) > 1 && (rules == nil || rules.Len() == 0) && m.useSchedulerFastPath() && !m.hasPluginScheduler() {
			for _, p := range providers {
				for _, a := range tier {
					if canonicalSchedulingProvider(executorKeyFromAuth(a)) == canonicalSchedulingProvider(p) {
						chosen = a
						break
					}
				}
				if canonicalSchedulingProvider(executorKeyFromAuth(chosen)) == canonicalSchedulingProvider(p) {
					break
				}
			}
		}
	case *RoundRobinSelector:
		// Rule and affinity paths use the selector's identity cursor. Indexed picks
		// use scheduler cursors, which are inspected without creating shards.
		if _, affinity := m.Selector().(*SessionAffinitySelector); affinity || (rules != nil && rules.Len() > 0) || !m.useSchedulerFastPath() || m.hasPluginScheduler() {
			key := provider + ":" + canonicalModelKey(strategyModel)
			s.mu.Lock()
			last := s.lastPicked[key]
			s.mu.Unlock()
			chosen = tier[successorIndex(tier, last)]
		} else if m.scheduler != nil {
			m.scheduler.mu.Lock()
			last := ""
			chosenProvider := providers[0]
			if len(providers) > 1 {
				normalized := normalizeProviderKeys(providers)
				slot := m.scheduler.mixedCursors[strings.Join(normalized, ",")+":"+canonicalModelKey(model)] % len(tier)
				for _, providerKey := range normalized {
					count := 0
					for _, a := range tier {
						if canonicalSchedulingProvider(executorKeyFromAuth(a)) == providerKey {
							count++
						}
					}
					if slot < count {
						chosenProvider = providerKey
						break
					}
					slot -= count
				}
			}
			if p := m.scheduler.providers[canonicalSchedulingProvider(chosenProvider)]; p != nil {
				if shard := p.modelShards[canonicalModelKey(model)]; shard != nil {
					if bucket := shard.readyByPriority[authPriority(tier[0])]; bucket != nil {
						last = bucket.all.lastPicked
					}
				}
			}
			m.scheduler.mu.Unlock()
			providerTier := make([]*Auth, 0, len(tier))
			for _, a := range tier {
				if canonicalSchedulingProvider(executorKeyFromAuth(a)) == canonicalSchedulingProvider(chosenProvider) {
					providerTier = append(providerTier, a)
				}
			}
			if len(providerTier) > 0 {
				chosen = providerTier[successorIndex(providerTier, last)]
			}
		}
	}
	if chosen != nil {
		trace.Pick = capture.indexes[chosen.ID]
		trace.PickPredicted = true
		trace.DecidedBy = routing.DecidedByStrategy
	}
	return trace, nil
}
