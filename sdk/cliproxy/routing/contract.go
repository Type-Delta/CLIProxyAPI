// Package routing holds rule-based credential routing: the compiled rule engine,
// the per-selection routing trace recorded with analytics events, and the
// context plumbing that carries traces from credential selection to usage
// reporting.
//
// Precedence, weakest to strongest: custom rules (a lower rule wins over a
// higher one) < routing strategy (priority tier, then the configured picker)
// < session affinity (an eligible bound credential wins) < routing gate (hard
// eligibility checks that nothing can override).
package routing

import (
	"context"
	"net/http"
	"sync"
)

// TraceVersion is the schema version written to Trace.Version.
const TraceVersion = 1

// RequestAttrs are the request properties rule conditions are evaluated against.
type RequestAttrs struct {
	// Model is the model the client asked for, before alias or provider resolution.
	Model string
	// KeyID is config.APIKeyID of the client API key; empty for non-native access providers.
	KeyID string
	// ClientType is session.SessionInfo.ClientType ("claude", "codex", "opencode", "pi", "agy", "gemini", "generic", ...).
	ClientType string
	// Headers are the inbound request headers visible to selection.
	Headers http.Header
}

// Gate reason codes for Candidate.Gate. An empty Gate means the credential passed the gate.
const (
	GateDisabled     = "disabled"     // credential or model state disabled
	GateCooldown     = "cooldown"     // per-model cooldown / unavailable until Candidate.UntilMs
	GateQuota        = "quota"        // credential-wide quota exceeded
	GateUnauthorized = "unauthorized" // persistent auth failure or expired access token
	GateUnsupported  = "unsupported"  // model not served (unsupported, excluded, or force-model-prefix)
	GateRetryLimit   = "retry_limit"  // credential request-retry limit below the current retry round
	GateTried        = "tried"        // already selected in the current retry round
	GateWeight       = "weight"       // non-positive weight under weighted round-robin
	GatePolicy       = "policy"       // credential policy, auth kind, free-plan, or quota-reserve restriction
	GatePinned       = "pinned"       // request is pinned to a different credential
	GateGone         = "gone"         // referenced (e.g. bound) credential no longer exists
)

// Rule status codes for RuleTrace.Status.
const (
	RuleApplied     = "applied"      // bottom-most matching rule with at least one eligible target
	RuleOverridden  = "overridden"   // matched, but a lower matching rule applied
	RuleFellThrough = "fell_through" // matched, but none of its targets passed the gate
)

// Affinity states for AffinityStep.State.
const (
	AffinityOff    = "off"    // session affinity disabled
	AffinityNone   = "none"   // no session identity on the request
	AffinityNew    = "new"    // no usable binding; the pick becomes the new binding
	AffinityKept   = "kept"   // bound credential passed the gate and was used
	AffinityBroken = "broken" // bound credential failed the gate; session rebinds to the pick
	AffinityHeld   = "held"   // transient failover: binding kept, a different credential serves this attempt
)

// DecidedBy values for Trace.DecidedBy: the layer whose choice produced Trace.Pick.
const (
	DecidedByStrategy = "strategy"
	DecidedByAffinity = "affinity"
	DecidedByPin      = "pin"
	DecidedByPlugin   = "plugin"
	DecidedByCredits  = "credits"
	DecidedByHome     = "home"
	DecidedByNone     = "none" // no eligible credential
)

// Trace modes.
const (
	ModeLocal  = "local"
	ModeHome   = "home"
	ModeDryRun = "dry_run"
)

// Trace explains one credential selection. All cross references are indexes into Candidates.
// A Trace is immutable once attached to a context; build a new one per selection.
type Trace struct {
	Version    int    `json:"version"`
	Mode       string `json:"mode"`
	RetryRound int    `json:"retry_round"`
	// Selection is the 0-based count of selections made earlier for the same request.
	Selection  int           `json:"selection"`
	Strategy   string        `json:"strategy,omitempty"`
	DecidedBy  string        `json:"decided_by"`
	Candidates []Candidate   `json:"candidates"`
	Rules      *RulesStep    `json:"rules,omitempty"`
	Tier       *TierStep     `json:"tier,omitempty"`
	Affinity   *AffinityStep `json:"affinity,omitempty"`
	// Pick is the selected candidate, or -1 when no credential was selected.
	Pick int `json:"pick"`
	// PickPredicted is set by dry runs only; false means the strategy's pick could not be
	// predicted without side effects and Pick is -1 while Tier.Pool lists the choices.
	PickPredicted bool   `json:"pick_predicted,omitempty"`
	Error         string `json:"error,omitempty"`
}

// Candidate is one credential considered for the request.
type Candidate struct {
	// ID is Auth.ID in-process and in dry-run responses; persisted analytics store the
	// CPAUK credential_id pseudonym instead.
	ID       string `json:"id"`
	Provider string `json:"provider,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Gate     string `json:"gate,omitempty"`
	// UntilMs is the Unix time in milliseconds when a cooldown/quota gate is expected to clear.
	UntilMs int64 `json:"until_ms,omitempty"`
	// Label is resolved from currently loaded credentials when serving API responses. Never persisted.
	Label string `json:"label,omitempty"`
	// AuthIndex is Auth.EnsureIndex(), used in-process to derive the analytics credential_id. Never serialized.
	AuthIndex string `json:"-"`
}

// RulesStep records custom-rule evaluation.
type RulesStep struct {
	// Matched lists enabled rules whose conditions matched, in config order.
	Matched []RuleTrace `json:"matched"`
	// Applied is the config index of the applied rule, or -1 when no rule applied.
	Applied int `json:"applied"`
	// Pool lists the eligible candidates left after narrowing; empty when no rule applied.
	Pool []int `json:"pool,omitempty"`
}

// RuleTrace describes one matching rule.
type RuleTrace struct {
	Index int    `json:"index"`
	Name  string `json:"name,omitempty"`
	// Condition is the rule's condition rendered as one line, at most 240 characters.
	Condition string `json:"condition,omitempty"`
	// Targets lists the rule's route-to credentials present among Candidates.
	Targets []int  `json:"targets"`
	Status  string `json:"status"`
}

// TierStep is the strategy input: the highest priority tier of the (possibly narrowed) pool.
type TierStep struct {
	Priority int   `json:"priority"`
	Pool     []int `json:"pool"`
}

// AffinityStep records the session-affinity outcome.
type AffinityStep struct {
	State string `json:"state"`
	// Bound is the previously bound candidate, or -1.
	Bound int `json:"bound"`
	// AgeMs is how long ago the binding was created, when known.
	AgeMs int64 `json:"age_ms,omitempty"`
}

// RuleMatch is one matching rule as returned by RuleSet.Decide.
type RuleMatch struct {
	Index     int
	Name      string
	Condition string
	// Targets are the rule's configured route-to Auth IDs.
	Targets []string
	Status  string
}

// Decision is the result of evaluating custom rules for one selection.
type Decision struct {
	// Matched lists enabled matching rules in config order, with Status filled in.
	Matched []RuleMatch
	// Applied is the config index of the applied rule, or -1.
	Applied int
	// Allowed is the subset of eligible IDs the applied rule narrows to; nil keeps the full pool.
	Allowed []string
}

// DryRunRequest describes a simulated request for Manager.DryRunRoute.
type DryRunRequest struct {
	Model      string
	KeyID      string
	ClientType string
	Headers    http.Header
	// SessionID, when set, is looked up read-only in the session-affinity store.
	SessionID string
	// Rules overrides the live rules (for unsaved drafts); nil uses the live rules.
	Rules *RuleSet
}

type traceKey struct{}

type holderKey struct{}

// WithTrace attaches an immutable selection trace to an attempt context.
func WithTrace(ctx context.Context, t *Trace) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, t)
}

// TraceFromContext returns the selection trace attached to an attempt context, or nil.
func TraceFromContext(ctx context.Context) *Trace {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(traceKey{}).(*Trace)
	return t
}

// TraceHolder keeps the latest selection trace of a request, including failed
// selections that never produce an attempt context. Its presence on the request
// context is what enables trace capture.
type TraceHolder struct {
	mu   sync.Mutex
	last *Trace
}

// Set records the latest selection trace.
func (h *TraceHolder) Set(t *Trace) {
	if h == nil || t == nil {
		return
	}
	h.mu.Lock()
	h.last = t
	h.mu.Unlock()
}

// Last returns the latest selection trace, or nil.
func (h *TraceHolder) Last() *Trace {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

// WithTraceHolder installs a request-scoped TraceHolder, enabling trace capture.
func WithTraceHolder(ctx context.Context) (context.Context, *TraceHolder) {
	h := &TraceHolder{}
	return context.WithValue(ctx, holderKey{}, h), h
}

// HolderFromContext returns the request's TraceHolder, or nil when trace capture is off.
func HolderFromContext(ctx context.Context) *TraceHolder {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(holderKey{}).(*TraceHolder)
	return h
}
