package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

type TokenQuality string

const (
	TokenQualityExact     TokenQuality = "exact"
	TokenQualityEstimated TokenQuality = "estimated"
	TokenQualityMissing   TokenQuality = "missing"
)

func (q TokenQuality) Valid() bool {
	return q == TokenQualityExact || q == TokenQualityEstimated || q == TokenQualityMissing
}

type TokenUsage struct {
	Input         int64        `json:"input"`
	Output        int64        `json:"output"`
	Reasoning     int64        `json:"reasoning"`
	Cached        int64        `json:"cached"`
	CacheRead     int64        `json:"cache_read"`
	CacheCreation int64        `json:"cache_creation"`
	Total         int64        `json:"total"`
	Schema        string       `json:"accounting_schema"`
	Quality       TokenQuality `json:"quality"`
}

func (t TokenUsage) UnclassifiedTokens() int64 {
	remaining := t.Total - t.Input
	if remaining <= 0 {
		return 0
	}
	remaining -= t.Output
	if remaining <= 0 {
		return 0
	}
	return remaining
}

type Event struct {
	SchemaVersion         int              `json:"schema_version"`
	AttemptID             string           `json:"attempt_id"`
	ProxyRequestID        string           `json:"proxy_request_id"`
	RequestIDQuality      RequestIDQuality `json:"request_id_quality"`
	KeyID                 string           `json:"key_id"`
	RequestedAt           time.Time        `json:"requested_at"`
	Provider              string           `json:"provider"`
	ExecutorType          string           `json:"executor_type"`
	Model                 string           `json:"model"`
	RequestedAlias        *string          `json:"requested_alias"`
	EndpointClass         string           `json:"endpoint_class"`
	AuthType              *string          `json:"auth_type"`
	CredentialID          *string          `json:"credential_id"`
	CredentialIDAlgorithm *string          `json:"credential_id_algorithm"`
	Succeeded             bool             `json:"succeeded"`
	UpstreamStatusCode    *int             `json:"upstream_status_code"`
	ErrorClass            *string          `json:"error_class"`
	LatencyMS             int64            `json:"latency_ms"`
	RoutingTimeMS         *int64           `json:"routing_time_ms,omitempty"`
	FirstTokenLatencyMS   *int64           `json:"first_token_latency_ms,omitempty"`
	ProviderLatencyMS     *int64           `json:"provider_latency_ms,omitempty"`
	GenerationTimeMS      *int64           `json:"generation_time_ms,omitempty"`
	TimeToFirstTokenMS    *int64           `json:"time_to_first_token_ms"`
	ServiceTierRequested  *string          `json:"service_tier_requested"`
	ServiceTierUsed       *string          `json:"service_tier_used"`
	Generated             bool             `json:"generated"`
	Tokens                TokenUsage       `json:"tokens"`
	// Hop diagnostics describe each leg of Client -> CPA -> Provider -> CPA -> Client.
	// They are additive and nullable; events recorded before they existed carry none.
	ClientMethod      *string    `json:"client_method"`
	ClientPath        *string    `json:"client_path"`
	ReceivedAt        *time.Time `json:"received_at"`
	UpstreamMethod    *string    `json:"upstream_method"`
	UpstreamURL       *string    `json:"upstream_url"`
	UpstreamSentAt    *time.Time `json:"upstream_sent_at"`
	UpstreamUsageRaw  *RawJSON   `json:"upstream_usage_raw"`
	UpstreamErrorBody *string    `json:"upstream_error_body"`
	ProxyStatusCode   *int       `json:"proxy_status_code"`
	ProxyError        *string    `json:"proxy_error"`
	RespondedAt       *time.Time `json:"responded_at"`
	// Query-time pricing enrichment is omitted from sanitized intake events and
	// populated only when an event is read from durable storage.
	KnownCost      *NanoUSD `json:"known_cost_usd,omitempty"`
	UnpricedTokens int64    `json:"unpriced_tokens,omitempty"`
	PriceRuleID    string   `json:"price_rule_id,omitempty"`
	PriceSource    string   `json:"price_source,omitempty"`
	ImportBatchID  string   `json:"import_batch_id,omitempty"`
	Source         string   `json:"source,omitempty"`
}

func (e Event) Validate() error {
	if e.SchemaVersion != EventSchemaVersion {
		return fmt.Errorf("unsupported event schema version %d", e.SchemaVersion)
	}
	if !IsCorrelationID(e.AttemptID) || !IsCorrelationID(e.ProxyRequestID) {
		return fmt.Errorf("attempt and proxy request IDs must be lowercase 128-bit hex")
	}
	if e.RequestIDQuality != RequestIDObserved && e.RequestIDQuality != RequestIDSynthetic {
		return fmt.Errorf("invalid request ID quality %q", e.RequestIDQuality)
	}
	if !IsFullKeyID(e.KeyID) {
		return fmt.Errorf("invalid key ID")
	}
	if e.RequestedAt.IsZero() || e.RequestedAt.Location() != time.UTC {
		return fmt.Errorf("requested_at must be a nonzero UTC timestamp")
	}
	for name, value := range map[string]string{
		"provider": e.Provider, "executor_type": e.ExecutorType, "model": e.Model,
		"endpoint_class": e.EndpointClass,
	} {
		if err := validateBoundedString(name, value, false); err != nil {
			return err
		}
	}
	for name, value := range map[string]*string{
		"requested_alias": e.RequestedAlias, "auth_type": e.AuthType,
		"error_class": e.ErrorClass, "service_tier_requested": e.ServiceTierRequested,
		"service_tier_used": e.ServiceTierUsed,
	} {
		if value != nil {
			if err := validateBoundedString(name, *value, true); err != nil {
				return err
			}
		}
	}
	if e.CredentialID == nil != (e.CredentialIDAlgorithm == nil) {
		return fmt.Errorf("credential ID and algorithm must both be null or present")
	}
	if e.CredentialID != nil {
		if !IsFullKeyID(*e.CredentialID) || *e.CredentialIDAlgorithm != CredentialIDAlgorithm {
			return fmt.Errorf("invalid credential identity")
		}
	}
	if e.UpstreamStatusCode != nil && (*e.UpstreamStatusCode < 100 || *e.UpstreamStatusCode > 599) {
		return fmt.Errorf("invalid upstream status code")
	}
	if e.ProxyStatusCode != nil && (*e.ProxyStatusCode < 100 || *e.ProxyStatusCode > 599) {
		return fmt.Errorf("invalid proxy status code")
	}
	for name, field := range map[string]struct {
		value *string
		limit int
	}{
		"client_method": {e.ClientMethod, 16}, "client_path": {e.ClientPath, MaxStoredStringBytes},
		"upstream_method": {e.UpstreamMethod, 16}, "upstream_url": {e.UpstreamURL, 512},
		"upstream_error_body": {e.UpstreamErrorBody, MaxRawPayloadBytes},
		"proxy_error":         {e.ProxyError, MaxProxyErrorBytes},
	} {
		if field.value == nil {
			continue
		}
		if !utf8.ValidString(*field.value) || len(*field.value) > field.limit {
			return fmt.Errorf("%s must be valid UTF-8 within %d bytes", name, field.limit)
		}
	}
	if e.UpstreamUsageRaw != nil && (!utf8.ValidString(string(*e.UpstreamUsageRaw)) || len(*e.UpstreamUsageRaw) > MaxRawGenerationBytes) {
		return fmt.Errorf("upstream_usage_raw must be valid UTF-8 within %d bytes", MaxRawGenerationBytes)
	}
	for name, value := range map[string]*time.Time{
		"received_at": e.ReceivedAt, "upstream_sent_at": e.UpstreamSentAt, "responded_at": e.RespondedAt,
	} {
		if value != nil && (value.IsZero() || value.Location() != time.UTC) {
			return fmt.Errorf("%s must be a nonzero UTC timestamp", name)
		}
	}
	if e.GenerationTimeMS != nil && *e.GenerationTimeMS < 0 {
		return fmt.Errorf("generation time must not be negative")
	}
	if e.RoutingTimeMS != nil && *e.RoutingTimeMS < 0 || e.FirstTokenLatencyMS != nil && *e.FirstTokenLatencyMS < 0 || e.ProviderLatencyMS != nil && *e.ProviderLatencyMS < 0 || e.LatencyMS < 0 || e.TimeToFirstTokenMS != nil && *e.TimeToFirstTokenMS < 0 {
		return fmt.Errorf("latency values must not be negative")
	}
	if err := e.Tokens.Validate(); err != nil {
		return err
	}
	if e.KnownCost != nil && *e.KnownCost < 0 || e.UnpricedTokens < 0 {
		return fmt.Errorf("event pricing values must not be negative")
	}
	encoded, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	if len(encoded) > MaxEventBytes {
		return fmt.Errorf("event exceeds %d bytes", MaxEventBytes)
	}
	return nil
}

func (t TokenUsage) Validate() error {
	for name, value := range map[string]int64{
		"input": t.Input, "output": t.Output, "reasoning": t.Reasoning,
		"cached": t.Cached, "cache_read": t.CacheRead,
		"cache_creation": t.CacheCreation, "total": t.Total,
	} {
		if value < 0 {
			return fmt.Errorf("token value %s must not be negative", name)
		}
	}
	if strings.TrimSpace(t.Schema) == "" || len(t.Schema) > MaxStoredStringBytes {
		return fmt.Errorf("invalid accounting schema")
	}
	if t.Quality != TokenQualityExact && t.Quality != TokenQualityEstimated && t.Quality != TokenQualityMissing {
		return fmt.Errorf("invalid token quality %q", t.Quality)
	}
	return nil
}

func validateBoundedString(name, value string, nullable bool) error {
	if !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must be trimmed valid UTF-8", name)
	}
	if !nullable && value == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	if len([]byte(value)) > MaxStoredStringBytes {
		return fmt.Errorf("%s exceeds %d bytes", name, MaxStoredStringBytes)
	}
	return nil
}

// ProxyResponsePatch completes the CPA -> Client leg for every attempt that
// shares one proxy request ID.
type ProxyResponsePatch struct {
	ProxyRequestID string
	StatusCode     int
	Error          string
	RespondedAt    time.Time
}

// RawJSON is provider payload text captured verbatim. It serializes as a JSON
// value when the text is valid JSON and as a JSON string otherwise, so clients
// can render structured usage without choking on truncated or non-JSON bodies.
type RawJSON string

func (r RawJSON) MarshalJSON() ([]byte, error) {
	if json.Valid([]byte(r)) {
		return []byte(r), nil
	}
	return json.Marshal(string(r))
}

func (r *RawJSON) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*r = RawJSON(text)
		return nil
	}
	*r = RawJSON(data)
	return nil
}
