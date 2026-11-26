package auth

import (
	"context"
	"maps"
	"strconv"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/routing"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

type routingPoolKey struct{}

type routingSnapshot struct {
	rules       *routing.RuleSet
	needsClient bool
}

func (m *Manager) compiledRoutingRules() *routing.RuleSet {
	if snapshot := m.routingRules.Load(); snapshot != nil {
		return snapshot.rules
	}
	return nil
}

func routingRulesNeedClient(rules []internalconfig.RoutingRule) bool {
	var needsClient func(*internalconfig.RoutingCondition) bool
	needsClient = func(c *internalconfig.RoutingCondition) bool {
		if c == nil {
			return false
		}
		if c.Client != nil || needsClient(c.Not) {
			return true
		}
		for i := range c.All {
			if needsClient(&c.All[i]) {
				return true
			}
		}
		for i := range c.Any {
			if needsClient(&c.Any[i]) {
				return true
			}
		}
		return false
	}
	for _, rule := range rules {
		if (rule.Enabled == nil || *rule.Enabled) && needsClient(rule.When) {
			return true
		}
	}
	return false
}

type routingPool struct {
	decide       func(routing.RequestAttrs, []string) routing.Decision
	attrs        routing.RequestAttrs
	opts         cliproxyexecutor.Options
	needsClient  bool
	clientLoaded bool
	lastNarrowed bool
	applied      int
}

func (m *Manager) routingPoolForRequest(ctx context.Context, model string, opts cliproxyexecutor.Options) *routingPool {
	if pinnedAuthIDFromMetadata(opts.Metadata) != "" || credentialPolicyFromContext(ctx) != "" {
		return nil
	}
	if pool, ok := ctx.Value(routingPoolKey{}).(*routingPool); ok {
		return pool
	}
	snapshot := m.routingRules.Load()
	if snapshot == nil || snapshot.rules == nil || snapshot.rules.Len() == 0 {
		return nil
	}
	// The route model is the best available fallback for SDK callers that omit
	// requested-model metadata.
	attrs := routing.RequestAttrs{Model: requestedModelAliasFromOptions(opts, model), Headers: opts.Headers}
	attrs.KeyID, _ = opts.Metadata[cliproxyexecutor.ClientKeyIDMetadataKey].(string)
	return &routingPool{decide: snapshot.rules.Decide, attrs: attrs, opts: opts, needsClient: snapshot.needsClient}
}

func narrowRoutingPool(ctx context.Context, auths []*Auth) []*Auth {
	pool, _ := ctx.Value(routingPoolKey{}).(*routingPool)
	if pool == nil {
		return auths
	}
	pool.lastNarrowed = false
	if len(auths) == 0 {
		return auths
	}
	ids := make([]string, 0, len(auths))
	for _, auth := range auths {
		ids = append(ids, auth.ID)
	}
	if pool.needsClient && !pool.clientLoaded {
		if info, ok := cliproxysession.ExtractSessionInfo(pool.opts.Headers, pool.opts.OriginalRequest, pool.opts.Metadata); ok {
			pool.attrs.ClientType = info.ClientType
		}
		pool.clientLoaded = true
	}
	decision := pool.decide(pool.attrs, ids)
	pool.lastNarrowed = decision.Allowed != nil
	pool.applied = decision.Applied
	if decision.Allowed == nil {
		captureRoutingDecision(ctx, decision, auths)
		return auths
	}
	allowed := make(map[string]struct{}, len(decision.Allowed))
	for _, id := range decision.Allowed {
		allowed[id] = struct{}{}
	}
	narrowed := make([]*Auth, 0, len(decision.Allowed))
	for _, auth := range auths {
		if _, ok := allowed[auth.ID]; ok {
			narrowed = append(narrowed, auth)
		}
	}
	captureRoutingDecision(ctx, decision, narrowed)
	return narrowed
}

func appliedRoutingPool(ctx context.Context) *routingPool {
	if ctx != nil {
		if pool, _ := ctx.Value(routingPoolKey{}).(*routingPool); pool != nil && pool.lastNarrowed {
			return pool
		}
	}
	return nil
}

func ruleStrategyKey(ctx context.Context, key string) string {
	if pool := appliedRoutingPool(ctx); pool != nil {
		return "\x00rule/" + strconv.Itoa(pool.applied) + "/" + key
	}
	return key
}

// routingStrategyAuths prepares selection-only copies for a rule-selected pool.
// All weights are equal inputs to the picker. The original auth is used for execution.
func routingStrategyAuths(ctx context.Context, auths []*Auth) []*Auth {
	if appliedRoutingPool(ctx) == nil {
		return highestPriorityAuths(auths)
	}
	normalized := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if authPriority(auth) == 0 && authWeight(auth) == 1 {
			normalized = append(normalized, auth)
			continue
		}
		candidate := *auth
		candidate.Attributes = maps.Clone(auth.Attributes)
		if candidate.Attributes == nil {
			candidate.Attributes = make(map[string]string, 1)
		}
		delete(candidate.Attributes, "priority")
		candidate.Attributes[AttributeWeight] = "1"
		normalized = append(normalized, &candidate)
	}
	return normalized
}

func pickRoutingStrategy(ctx context.Context, selector Selector, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if _, affinity := selector.(*SessionAffinitySelector); affinity {
		return selector.Pick(ctx, provider, selectionArgForSelector(selector, model), opts, auths)
	}
	picked, err := selector.Pick(ctx, provider, selectionArgForSelector(selector, model), opts, routingStrategyAuths(ctx, auths))
	if picked != nil && appliedRoutingPool(ctx) != nil {
		picked = pickSchedulerAuthByID(auths, picked.ID)
	}
	return picked, err
}
