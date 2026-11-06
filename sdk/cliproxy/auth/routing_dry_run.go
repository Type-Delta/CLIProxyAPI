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
	if req.DownstreamWebsocket {
		ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
	}
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
	keptPick := -1
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
				captureAffinityBinding(ctx, affinity.cache, key)
				captureAffinity(ctx, routing.AffinityBroken, entry.authID)
				for _, a := range eligible {
					if a.ID == entry.authID {
						captureAffinity(ctx, routing.AffinityKept, a.ID)
						captureAffinityRules(ctx, eligible)
						keptPick = capture.indexes[a.ID]
					}
				}
				if keptPick >= 0 {
					break
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
	keptRules := trace.Rules
	if rules != nil && rules.Len() > 0 {
		eligible = narrowRoutingPool(ctx, eligible)
	}
	if keptPick >= 0 {
		trace.Rules = keptRules
	}

	legacy := affinityEnabled || (rules != nil && rules.Len() > 0)
	for _, a := range eligible {
		if m.routeAwareSelectionRequired(a, model) {
			legacy = true
			break
		}
	}
	preferWS := cliproxyexecutor.DownstreamWebsocket(ctx) && len(providers) == 1 && providerPrefersWebsocketTransport(provider)
	tierCandidates := eligible
	if preferWS && !legacy && m.useSchedulerFastPath() {
		tierCandidates = preferWebsocketAuths(ctx, provider, tierCandidates)
	}
	tier := highestPriorityAuths(tierCandidates)
	if preferWS {
		tier = preferCodexWebsocketAuths(ctx, provider, tier)
	}
	if len(tier) == 0 {
		if keptPick >= 0 {
			trace.Pick = keptPick
			trace.PickPredicted = true
			trace.DecidedBy = routing.DecidedByAffinity
		} else {
			trace.Error = "no_eligible"
		}
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
		if keptPick >= 0 {
			trace.Pick = keptPick
			trace.PickPredicted = true
			trace.DecidedBy = routing.DecidedByAffinity
		}
		return trace, nil
	}
	step, sim := m.dryRunStrategy(capture, selector, tier, providers, provider, model, strategyModel, legacy, preferWS)
	trace.StrategyStep = step
	if step != nil {
		for range 10 {
			if a := sim.next(); a != nil {
				step.Next = append(step.Next, capture.candidateIndex(a))
			}
		}
		if len(step.Next) > 0 {
			step.Wanted = step.Next[0]
			trace.Pick = step.Wanted
			trace.PickPredicted = true
		}
		if step.Fallback == "missing_recovery_data" {
			step.Fallback = routing.FallbackMissingRecoveryDataRefreshPending
			trace.PickPredicted = false
		}
	}
	if keptPick >= 0 {
		trace.Pick = keptPick
		trace.PickPredicted = true
		trace.DecidedBy = routing.DecidedByAffinity
	}
	return trace, nil
}

func (m *Manager) dryRunStrategy(c *routingCapture, selector Selector, tier []*Auth, providers []string, provider, model, strategyModel string, legacy, preferWS bool) (*routing.StrategyStep, *strategySimulation) {
	if !legacy && m.useSchedulerFastPath() && m.scheduler != nil {
		// Build ready views privately, including deadlines that have expired since the
		// live shard was last visited. Copy only cursor state from the live scheduler.
		local := newAuthScheduler(selector)
		local.rebuild(tier)
		normalized := normalizeProviderKeys(providers)
		shards := make([]*modelScheduler, len(normalized))
		m.scheduler.mu.Lock()
		defer m.scheduler.mu.Unlock()
		key := canonicalModelKey(model)
		for i, p := range normalized {
			if ps := local.providers[p]; ps != nil {
				shards[i] = ps.ensureModelLocked(key, time.Now())
				if live := m.scheduler.providers[p]; live != nil {
					if shard := live.modelShards[key]; shard != nil {
						for priority, bucket := range shards[i].readyByPriority {
							if old := shard.readyByPriority[priority]; old != nil {
								restoreReadyViewCursors(&bucket.all, snapshotReadyViewCursors(old.all))
								restoreReadyViewCursors(&bucket.ws, snapshotReadyViewCursors(old.ws))
							}
						}
					}
				}
			}
		}
		for k, v := range m.scheduler.mixedCursors {
			local.mixedCursors[k] = v
		}
		for k, v := range m.scheduler.mixedWeightedStates {
			cp := cloneSmoothState(v)
			local.mixedWeightedStates[k] = &cp
		}
		predicate := scheduledAuthPredicate(authSelectionEligibility{}, nil, "", local.strategy == schedulerStrategyWeightedRoundRobin)
		return local.scheduledStrategySimulation(c, shards, preferWS, authPriority(tier[0]), predicate, local.strategy, normalized, model)
	}
	last := ""
	var state smoothWeightedState
	halfLife := time.Duration(0)
	key := provider + ":" + canonicalModelKey(strategyModel)
	switch s := selector.(type) {
	case *RoundRobinSelector:
		s.mu.Lock()
		last = s.lastPicked[key]
		s.mu.Unlock()
	case *WeightedRoundRobinSelector:
		key = provider + ":" + canonicalModelKey(model)
		s.mu.Lock()
		state = cloneSmoothState(s.states[key])
		s.mu.Unlock()
	case *GreedySoonestRecoverySelector:
		halfLife = s.recoveryHalfLife()
		key = strings.ToLower(strings.TrimSpace(provider)) + ":" + canonicalModelKey(strategyModel)
		s.mu.Lock()
		state = cloneSmoothState(s.states[key])
		fallback := s.fallback
		s.mu.Unlock()
		if fallback != nil {
			fallback.mu.Lock()
			last = fallback.lastPicked[provider+":"+canonicalModelKey(strategyModel)]
			fallback.mu.Unlock()
		}
	case *FillFirstSelector:
	default:
		return nil, nil
	}
	return strategyStep(c, c.trace.Strategy, tier, strategyModel, halfLife, last, &state)
}
