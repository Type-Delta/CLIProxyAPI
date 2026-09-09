package collector

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

const unknownValue = "unknown"

type Sanitizer struct {
	identityKey     [32]byte
	storeCredential bool
	now             func() time.Time
	newID           func() (string, error)
}

type SanitizerOptions struct {
	IdentityKey     [32]byte
	StoreCredential bool
	Now             func() time.Time
	NewID           func() (string, error)
}

type SanitizeResult struct {
	Event           model.Event
	TruncatedFields int64
}

type Source struct {
	ProxyRequestID string
	RequestQuality model.RequestIDQuality
	EndpointClass  string
	Provider       string
	ExecutorType   string
	Model          string
	Alias          string
	APIKey         string
	AuthID         string
	AuthIndex      string
	AuthType       string
	ServiceTier    string
	ResponseTier   string
	Generated      *bool
	RequestedAt    time.Time
	Latency        time.Duration
	TTFT           time.Duration
	GenerationTime *time.Duration
	Failed         bool
	StatusCode     int
	// UpstreamStatusCode is the observed provider status; zero when no provider request was made.
	UpstreamStatusCode int
	// FailureBody is the raw provider error body; it is bounded before storage.
	FailureBody string
	// Hop diagnostics; zero values mean "not observed".
	ClientMethod   string
	ClientPath     string
	ReceivedAt     time.Time
	UpstreamMethod string
	UpstreamURL    string
	UpstreamSentAt time.Time
	RawUsage       string
	Tokens         SourceTokens
}

type SourceTokens struct {
	Input         int64
	Output        int64
	Reasoning     int64
	Cached        int64
	CacheRead     int64
	CacheCreation int64
	Total         int64
	Quality       model.TokenQuality
}

func NewSanitizer(options SanitizerOptions) *Sanitizer {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	newID := options.NewID
	if newID == nil {
		newID = model.NewCorrelationID
	}
	return &Sanitizer{
		identityKey:     options.IdentityKey,
		storeCredential: options.StoreCredential,
		now:             now,
		newID:           newID,
	}
}

// Sanitize copies only Event v1 fields. It never serializes data, reads
// storage, or retains references to mutable usage values.
func (s *Sanitizer) Sanitize(record Source) (SanitizeResult, error) {
	if s == nil {
		return SanitizeResult{}, fmt.Errorf("sanitizer is nil")
	}
	if strings.TrimSpace(record.APIKey) == "" {
		return SanitizeResult{}, fmt.Errorf("api key is missing")
	}
	if record.RequestedAt.IsZero() {
		return SanitizeResult{}, fmt.Errorf("requested timestamp is missing")
	}
	if record.Latency < 0 || record.TTFT < 0 || record.GenerationTime != nil && *record.GenerationTime < 0 {
		return SanitizeResult{}, fmt.Errorf("latency is negative")
	}
	if err := validateTokens(record.Tokens); err != nil {
		return SanitizeResult{}, err
	}
	if record.StatusCode != 0 && (record.StatusCode < 100 || record.StatusCode > 599) {
		return SanitizeResult{}, fmt.Errorf("upstream status code is invalid")
	}

	attemptID, err := s.newID()
	if err != nil {
		return SanitizeResult{}, fmt.Errorf("generate attempt ID: %w", err)
	}
	requestQuality := model.RequestIDObserved
	proxyRequestID := record.ProxyRequestID
	if !model.IsCorrelationID(proxyRequestID) {
		proxyRequestID, err = s.newID()
		if err != nil {
			return SanitizeResult{}, fmt.Errorf("generate proxy request ID: %w", err)
		}
		requestQuality = model.RequestIDSynthetic
	} else if record.RequestQuality == model.RequestIDSynthetic {
		requestQuality = model.RequestIDSynthetic
	}

	truncated := int64(0)
	provider, count, err := bounded(record.Provider, true)
	truncated += count
	if err != nil {
		return SanitizeResult{}, fmt.Errorf("provider: %w", err)
	}
	executorType, count, err := bounded(record.ExecutorType, true)
	truncated += count
	if err != nil {
		return SanitizeResult{}, fmt.Errorf("executor type: %w", err)
	}
	modelName, count, err := bounded(record.Model, true)
	truncated += count
	if err != nil {
		return SanitizeResult{}, fmt.Errorf("model: %w", err)
	}
	alias, count, err := boundedOptional(record.Alias)
	truncated += count
	if err != nil {
		return SanitizeResult{}, fmt.Errorf("requested alias: %w", err)
	}
	authType, count, err := boundedOptional(record.AuthType)
	truncated += count
	if err != nil {
		return SanitizeResult{}, fmt.Errorf("auth type: %w", err)
	}
	requestedTier, count, err := boundedOptional(record.ServiceTier)
	truncated += count
	if err != nil {
		return SanitizeResult{}, fmt.Errorf("requested service tier: %w", err)
	}
	responseTier, count, err := boundedOptional(record.ResponseTier)
	truncated += count
	if err != nil {
		return SanitizeResult{}, fmt.Errorf("response service tier: %w", err)
	}

	var credentialID *string
	var credentialAlgorithm *string
	if s.storeCredential {
		credentialID, err = model.CredentialID(s.identityKey[:], provider, record.AuthIndex, record.AuthID)
		if err != nil {
			return SanitizeResult{}, fmt.Errorf("derive credential ID: %w", err)
		}
		if credentialID != nil {
			algorithm := model.CredentialIDAlgorithm
			credentialAlgorithm = &algorithm
		}
	}

	var statusCode *int
	if record.StatusCode != 0 {
		value := record.StatusCode
		statusCode = &value
	}
	hops, count := sanitizeHops(record)
	truncated += count

	var errorClass *string
	if record.Failed {
		value := classifyFailure(record.StatusCode)
		// A failure with no provider request is CPA's own decision (no usable
		// credential); do not attribute the status to the provider.
		if hops.upstreamSentAt == nil && record.UpstreamStatusCode == 0 && strings.EqualFold(record.ExecutorType, "auth-selection") {
			value = "auth_unavailable"
			statusCode = nil
			// The body is CPA's own message; the client-facing text arrives via the proxy response patch.
			hops.errorBody = nil
		}
		errorClass = &value
	}
	var ttft, generation *int64
	if record.GenerationTime != nil {
		value := record.GenerationTime.Milliseconds()
		generation = &value
	}
	if record.TTFT > 0 {
		value := record.TTFT.Milliseconds()
		ttft = &value
	}

	quality := record.Tokens.Quality
	if quality == "" {
		quality = model.TokenQualityExact
		if record.Tokens.Input == 0 && record.Tokens.Output == 0 && record.Tokens.Reasoning == 0 &&
			record.Tokens.Cached == 0 && record.Tokens.CacheRead == 0 && record.Tokens.CacheCreation == 0 && record.Tokens.Total == 0 {
			quality = model.TokenQualityMissing
		}
	}
	if !quality.Valid() {
		return SanitizeResult{}, fmt.Errorf("token quality is invalid")
	}

	event := model.Event{
		SchemaVersion:         model.EventSchemaVersion,
		AttemptID:             attemptID,
		ProxyRequestID:        proxyRequestID,
		RequestIDQuality:      requestQuality,
		KeyID:                 model.KeyID(record.APIKey),
		RequestedAt:           record.RequestedAt.UTC(),
		Provider:              provider,
		ExecutorType:          executorType,
		Model:                 modelName,
		RequestedAlias:        alias,
		EndpointClass:         classifyEndpoint(record.EndpointClass),
		AuthType:              authType,
		CredentialID:          credentialID,
		CredentialIDAlgorithm: credentialAlgorithm,
		Succeeded:             !record.Failed,
		UpstreamStatusCode:    statusCode,
		ErrorClass:            errorClass,
		LatencyMS:             record.Latency.Milliseconds(),
		TimeToFirstTokenMS:    ttft,
		GenerationTimeMS:      generation,
		ServiceTierRequested:  requestedTier,
		ServiceTierUsed:       responseTier,
		Generated:             generated(record.Generated),
		ClientMethod:          hops.clientMethod,
		ClientPath:            hops.clientPath,
		ReceivedAt:            hops.receivedAt,
		UpstreamMethod:        hops.upstreamMethod,
		UpstreamURL:           hops.upstreamURL,
		UpstreamSentAt:        hops.upstreamSentAt,
		UpstreamUsageRaw:      hops.rawUsage,
		UpstreamErrorBody:     hops.errorBody,
		Tokens: model.TokenUsage{
			Input:         record.Tokens.Input,
			Output:        record.Tokens.Output,
			Reasoning:     record.Tokens.Reasoning,
			Cached:        record.Tokens.Cached,
			CacheRead:     record.Tokens.CacheRead,
			CacheCreation: record.Tokens.CacheCreation,
			Total:         record.Tokens.Total,
			Schema:        "normalized-v1",
			Quality:       quality,
		},
	}
	return SanitizeResult{Event: event, TruncatedFields: truncated}, nil
}

func bounded(value string, required bool) (string, int64, error) {
	value = strings.TrimSpace(value)
	if value == "" && required {
		return unknownValue, 0, nil
	}
	if !utf8.ValidString(value) {
		return "", 0, fmt.Errorf("must be valid UTF-8")
	}
	if len(value) <= model.MaxStoredStringBytes {
		return value, 0, nil
	}
	value = value[:model.MaxStoredStringBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, 1, nil
}

type sanitizedHops struct {
	clientMethod, clientPath, upstreamMethod, upstreamURL, errorBody *string
	receivedAt, upstreamSentAt                                       *time.Time
	rawUsage                                                         *model.RawJSON
}

const truncationMarker = "...[truncated]"

// sanitizeHops bounds the hop diagnostics. Invalid UTF-8 or an upstream URL with
// a query string is dropped rather than failing the whole event.
func sanitizeHops(record Source) (sanitizedHops, int64) {
	var hops sanitizedHops
	truncated := int64(0)
	optional := func(value string, limit int, marker bool) *string {
		value = strings.TrimSpace(value)
		if value == "" || !utf8.ValidString(value) {
			return nil
		}
		if len(value) > limit {
			cut := limit
			if marker {
				cut -= len(truncationMarker)
			}
			value = value[:cut]
			for value != "" && !utf8.ValidString(value) {
				value = value[:len(value)-1]
			}
			if marker {
				value += truncationMarker
			}
			truncated++
		}
		return &value
	}
	utc := func(value time.Time) *time.Time {
		if value.IsZero() {
			return nil
		}
		value = value.UTC()
		return &value
	}
	hops.clientMethod = optional(record.ClientMethod, 16, false)
	hops.clientPath = optional(strings.SplitN(record.ClientPath, "?", 2)[0], model.MaxStoredStringBytes, false)
	hops.receivedAt = utc(record.ReceivedAt)
	hops.upstreamMethod = optional(record.UpstreamMethod, 16, false)
	hops.upstreamURL = optional(strings.SplitN(record.UpstreamURL, "?", 2)[0], 512, false)
	hops.upstreamSentAt = utc(record.UpstreamSentAt)
	if raw := optional(record.RawUsage, model.MaxRawPayloadBytes, false); raw != nil {
		value := model.RawJSON(*raw)
		hops.rawUsage = &value
	}
	if record.Failed {
		hops.errorBody = optional(record.FailureBody, model.MaxRawPayloadBytes, true)
	}
	return hops, truncated
}

func boundedOptional(value string) (*string, int64, error) {
	value, count, err := bounded(value, false)
	if err != nil || value == "" {
		return nil, count, err
	}
	return &value, count, nil
}

func validateTokens(detail SourceTokens) error {
	values := []int64{
		detail.Input, detail.Output, detail.Reasoning,
		detail.Cached, detail.CacheRead, detail.CacheCreation, detail.Total,
	}
	for _, value := range values {
		if value < 0 {
			return fmt.Errorf("token count is negative")
		}
	}
	return nil
}

func generated(value *bool) bool {
	return value == nil || *value
}

func classifyFailure(status int) string {
	switch {
	case status == 429:
		return "rate_limited"
	case status >= 500:
		return "upstream_unavailable"
	case status >= 400:
		return "upstream_rejected"
	case status >= 300:
		return "upstream_redirect"
	default:
		return "transport_failure"
	}
}

// middlewareEndpointClasses is the bounded class set the proxy middleware assigns.
var middlewareEndpointClasses = map[string]struct{}{
	"chat_completions": {}, "responses": {}, "messages": {}, "embeddings": {}, "images": {},
	"audio": {}, "videos": {}, "moderations": {}, "realtime": {}, "live": {}, "search": {},
	"gemini_generate": {}, "gemini_stream_generate": {}, "models": {}, "other": {},
	"generate_content": {},
}

func classifyEndpoint(endpoint string) string {
	endpoint = strings.ToLower(strings.TrimSpace(endpoint))
	if _, ok := middlewareEndpointClasses[endpoint]; ok {
		return endpoint
	}
	switch {
	case endpoint == "responses" || strings.Contains(endpoint, "/responses"):
		return "responses"
	case endpoint == "messages" || strings.Contains(endpoint, "/messages"):
		return "messages"
	case endpoint == "chat_completions" || strings.Contains(endpoint, "/chat/completions"):
		return "chat_completions"
	case endpoint == "generate_content" || strings.Contains(endpoint, "generatecontent"):
		return "generate_content"
	case endpoint == "embeddings" || strings.Contains(endpoint, "/embeddings"):
		return "embeddings"
	case endpoint == "images" || strings.Contains(endpoint, "/images"):
		return "images"
	case endpoint == "videos" || strings.Contains(endpoint, "/videos"):
		return "videos"
	case endpoint == "audio" || strings.Contains(endpoint, "/audio"):
		return "audio"
	default:
		return unknownValue
	}
}
