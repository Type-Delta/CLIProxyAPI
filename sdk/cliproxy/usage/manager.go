package usage

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultServiceTier is retained for direct SDK and non-OpenAI usage callers.
const DefaultServiceTier = "default"

// AutoServiceTier is the OpenAI request semantics when service_tier is omitted.
// OpenAI HTTP handlers set it explicitly, without changing other providers'
// historical direct-SDK default.
const AutoServiceTier = "auto"

// Record contains the usage statistics captured for a single provider request.
type Record struct {
	// ProxyRequestID correlates all upstream attempts made for one proxy request.
	ProxyRequestID string
	// RequestIDQuality reports whether ProxyRequestID came from proxy middleware
	// or was generated for a direct SDK record.
	RequestIDQuality RequestIDQuality
	Provider         string
	// ExecutorType stores the concrete executor type that handled the request.
	ExecutorType    string
	Model           string
	Alias           string
	APIKey          string
	SessionID       string
	ParentSessionID string
	AuthID          string
	AuthIndex       string
	// AccessTokenSHA256 identifies the OAuth token version without exposing the token.
	AccessTokenSHA256 string
	AuthType          string
	Source            string
	// EndpointClass is a bounded semantic route class, never a raw URL.
	EndpointClass string
	// ClientMethod and ClientPath describe the downstream request line (route path, never a query string).
	ClientMethod string
	ClientPath   string
	// ReceivedAt is when the proxy accepted the downstream request.
	ReceivedAt time.Time
	// UpstreamMethod and UpstreamURL describe the provider request; the URL never carries a query string.
	UpstreamMethod string
	UpstreamURL    string
	// UpstreamSentAt is the current provider dispatch, matching the local latency origin.
	UpstreamSentAt time.Time
	// UpstreamStatusCode is the provider HTTP status for both successful and failed attempts.
	UpstreamStatusCode int
	// ReasoningEffort stores the translated upstream thinking level for request event logs.
	ReasoningEffort string
	// ServiceTier stores the client-requested service tier.
	ServiceTier string
	// RequestServiceTier is a deprecated input-only alias retained for existing
	// plugin callers. It is normalized into ServiceTier and never emitted.
	RequestServiceTier string
	// ResponseServiceTier stores the final tier reported by the upstream response.
	ResponseServiceTier string
	// Generate reports whether the client requested actual generation.
	// nil or true means generation is enabled; only an explicit false disables generation.
	// Use GenerateFlag to set the value and GenerateEnabled to read it with the default.
	Generate *bool
	// Stream reports whether the request was executed in streaming mode.
	Stream      bool
	RequestedAt time.Time
	Latency     time.Duration
	TTFT        time.Duration
	// GenerationTime measures first-to-last substantive token arrivals on the local monotonic clock.
	// nil means the request did not expose observable streaming token boundaries.
	GenerationTime *time.Duration
	// FirstTokenLatency measures dispatch to the first substantive streaming token, without TTFT fallback.
	// nil means no substantive token was observed on a measured transport.
	FirstTokenLatency *time.Duration
	// RoutingTime measures request receipt to the first provider dispatch, shared across retries.
	RoutingTime *time.Duration
	// ProviderLatency measures dispatch to response headers or a WebSocket application frame, including network time.
	// nil means no response was observed on a measured transport.
	ProviderLatency *time.Duration
	Failed          bool
	Fail            Failure
	Detail          Detail
	// ResponseHeaders stores a snapshot of upstream response headers for usage sinks.
	ResponseHeaders http.Header
}

// Failure holds HTTP failure metadata for an upstream request attempt.
type Failure struct {
	StatusCode int
	Body       string
}

// Detail holds the token usage breakdown.
type Detail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
	TokenBreakdown      TokenBreakdown
	ResponseServiceTier string
	// TokenQuality distinguishes exact upstream usage from estimates and
	// responses where token data was unavailable. An empty value is inferred.
	TokenQuality TokenQuality
	// RawUsage holds the provider usage node the detail was parsed from, bounded to MaxRawUsageBytes.
	// It is a string so Detail stays comparable.
	RawUsage string
}

// MaxRawUsageBytes bounds Detail.RawUsage.
const MaxRawUsageBytes = 40 * 1024

// MaxUpstreamURLBytes bounds Record.UpstreamURL.
const MaxUpstreamURLBytes = 512

// TokenQuality describes the reliability of normalized token accounting.
type TokenQuality string

const (
	TokenQualityExact     TokenQuality = "exact"
	TokenQualityEstimated TokenQuality = "estimated"
	TokenQualityMissing   TokenQuality = "missing"
)

type requestedModelAliasContextKey struct{}
type reasoningEffortContextKey struct{}
type serviceTierContextKey struct{}
type generateContextKey struct{}
type streamContextKey struct{}
type endpointClassContextKey struct{}
type requestReceivedAtContextKey struct{}
type clientRequestLineContextKey struct{}

type requestRoutingTiming struct {
	receivedAt time.Time
	once       sync.Once
	duration   time.Duration
}

type clientRequestLine struct {
	method string
	path   string
}

// WithRequestReceivedAt stores when the proxy accepted the downstream request.
func WithRequestReceivedAt(ctx context.Context, receivedAt time.Time) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if receivedAt.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, requestReceivedAtContextKey{}, &requestRoutingTiming{receivedAt: receivedAt})
}

// RequestReceivedAtFromContext returns the downstream request arrival time, or zero.
func RequestReceivedAtFromContext(ctx context.Context) time.Time {
	if ctx == nil {
		return time.Time{}
	}
	timing, _ := ctx.Value(requestReceivedAtContextKey{}).(*requestRoutingTiming)
	if timing == nil {
		return time.Time{}
	}
	return timing.receivedAt
}

// InheritRequestTiming preserves the shared first-dispatch observation across execution contexts.
func InheritRequestTiming(ctx, source context.Context) context.Context {
	if source == nil {
		return ctx
	}
	if timing, ok := source.Value(requestReceivedAtContextKey{}).(*requestRoutingTiming); ok {
		return context.WithValue(ctx, requestReceivedAtContextKey{}, timing)
	}
	return ctx
}

// ObserveRequestRouting records receipt to the first dispatch once for all request attempts.
func ObserveRequestRouting(ctx context.Context, dispatchAt time.Time) *time.Duration {
	if ctx == nil {
		return nil
	}
	timing, _ := ctx.Value(requestReceivedAtContextKey{}).(*requestRoutingTiming)
	if timing == nil || dispatchAt.Before(timing.receivedAt) {
		return nil
	}
	timing.once.Do(func() { timing.duration = dispatchAt.Sub(timing.receivedAt) })
	duration := timing.duration
	return &duration
}

// WithClientRequestLine stores the downstream HTTP method and route path (never a query string).
func WithClientRequestLine(ctx context.Context, method, path string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	method = truncateUTF8(strings.TrimSpace(method), 16)
	path = truncateUTF8(strings.TrimSpace(path), 256)
	if method == "" && path == "" {
		return ctx
	}
	return context.WithValue(ctx, clientRequestLineContextKey{}, clientRequestLine{method: method, path: path})
}

// ClientRequestLineFromContext returns the downstream method and path stored in ctx.
func ClientRequestLineFromContext(ctx context.Context) (method, path string) {
	if ctx == nil {
		return "", ""
	}
	line, _ := ctx.Value(clientRequestLineContextKey{}).(clientRequestLine)
	return line.method, line.path
}

// WithRequestedModelAlias stores the client-requested model name for usage sinks.
func WithRequestedModelAlias(ctx context.Context, alias string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return ctx
	}
	return context.WithValue(ctx, requestedModelAliasContextKey{}, alias)
}

// RequestedModelAliasFromContext returns the client-requested model name stored in ctx.
func RequestedModelAliasFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(requestedModelAliasContextKey{})
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

// WithReasoningEffort stores the client-requested reasoning effort for usage sinks.
func WithReasoningEffort(ctx context.Context, effort string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	effort = strings.TrimSpace(effort)
	if effort == "" {
		return ctx
	}
	return context.WithValue(ctx, reasoningEffortContextKey{}, effort)
}

// ReasoningEffortFromContext returns the client-requested reasoning effort stored in ctx.
func ReasoningEffortFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(reasoningEffortContextKey{})
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

// WithServiceTier stores the client-requested service tier for usage sinks.
func WithServiceTier(ctx context.Context, tier string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	tier = strings.TrimSpace(tier)
	if tier == "" {
		tier = DefaultServiceTier
	}
	return context.WithValue(ctx, serviceTierContextKey{}, tier)
}

// ServiceTierFromContext returns the client-requested service tier stored in ctx.
func ServiceTierFromContext(ctx context.Context) string {
	if ctx == nil {
		return DefaultServiceTier
	}
	raw := ctx.Value(serviceTierContextKey{})
	switch value := raw.(type) {
	case string:
		tier := strings.TrimSpace(value)
		if tier == "" {
			return DefaultServiceTier
		}
		return tier
	case []byte:
		tier := strings.TrimSpace(string(value))
		if tier == "" {
			return DefaultServiceTier
		}
		return tier
	default:
		return DefaultServiceTier
	}
}

// WithGenerate stores whether the client requested actual generation for usage sinks.
// Missing context values default to true; only an explicit false disables generation.
func WithGenerate(ctx context.Context, generate bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, generateContextKey{}, generate)
}

// GenerateFromContext returns whether the client requested actual generation.
// Missing values default to true.
func GenerateFromContext(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	raw := ctx.Value(generateContextKey{})
	switch value := raw.(type) {
	case bool:
		return value
	default:
		return true
	}
}

// WithStream stores whether the request was executed in streaming mode for usage sinks.
func WithStream(ctx context.Context, stream bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, streamContextKey{}, stream)
}

// StreamFromContext returns whether the request was executed in streaming mode.
// Missing values default to false.
func StreamFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	raw := ctx.Value(streamContextKey{})
	switch value := raw.(type) {
	case bool:
		return value
	default:
		return false
	}
}

// GenerateFlag returns a pointer suitable for Record.Generate.
func GenerateFlag(generate bool) *bool {
	return &generate
}

// GenerateEnabled reports whether generation is enabled for the record field.
// A nil value defaults to true so legacy callers that omit Generate keep the historical behavior.
func GenerateEnabled(generate *bool) bool {
	if generate == nil {
		return true
	}
	return *generate
}

// WithEndpointClass stores a semantic endpoint class for usage records.
func WithEndpointClass(ctx context.Context, endpointClass string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	endpointClass = strings.TrimSpace(endpointClass)
	if endpointClass == "" {
		return ctx
	}
	endpointClass = truncateUTF8(endpointClass, 256)
	return context.WithValue(ctx, endpointClassContextKey{}, endpointClass)
}

// EndpointClassFromContext returns the semantic endpoint class stored in ctx.
func EndpointClassFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	endpointClass, _ := ctx.Value(endpointClassContextKey{}).(string)
	endpointClass = strings.TrimSpace(endpointClass)
	return truncateUTF8(endpointClass, 256)
}
