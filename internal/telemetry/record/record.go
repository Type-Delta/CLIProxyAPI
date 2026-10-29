// Package record defines the capture contract between CPA's request path and
// the ClickHouse telemetry ingester. A Request is one client request (one proxy
// turn) with every upstream attempt it made. Producers write a Request to the
// telemetry outbox; the ingester parses transcripts out of the bodies and
// stores them deduplicated, so producers must pass bodies through unmodified.
//
// Secrets never enter a Request: headers and URLs are masked by the producer
// and client API keys appear only as their pseudonymous key ID.
package record

import (
	"errors"
	"time"
)

// ErrIngestPaused is wrapped by sinks that cannot accept records right now
// (ClickHouse unreachable or low on disk). Callers retry later with errors.Is.
var ErrIngestPaused = errors.New("telemetry ingestion paused")

// Detail levels describe why a request was captured.
const (
	// DetailFull is a request captured because detailed request logging is on.
	DetailFull = "full"
	// DetailError is a forced capture of a failed request while detailed
	// request logging is off.
	DetailError = "error"
)

// Sources describe where a Request came from.
const (
	SourceLive      = "live"
	SourceLegacyLog = "legacy_log"
)

// Transports describe how a request or attempt reached its peer.
const (
	TransportHTTP      = "http"
	TransportWebsocket = "websocket"
)

// Request is one client request and its upstream attempts.
type Request struct {
	// RequestID is the request-log ID (UUIDv7) assigned by the Gin logger.
	RequestID string `json:"request_id"`
	// ProxyRequestID joins analytics events (analytics usage events use the
	// same proxy_request_id). Empty when unknown.
	ProxyRequestID string    `json:"proxy_request_id,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	// FirstChunkAt is when the first response byte reached the client.
	FirstChunkAt time.Time `json:"first_chunk_at,omitzero"`
	FinishedAt   time.Time `json:"finished_at,omitzero"`

	Method string `json:"method"`
	// URL is the client-facing URL with sensitive query values masked.
	URL string `json:"url"`
	// ClientFormat is an optional hint ("openai-chat", "openai-responses",
	// "claude", "gemini"). The ingester detects the format when empty.
	ClientFormat string `json:"client_format,omitempty"`
	Transport    string `json:"transport,omitempty"`

	// ActorKeyID is the pseudonymous client key ID (config.APIKeyID).
	ActorKeyID string `json:"actor_key_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`

	Detail string `json:"detail"`
	Source string `json:"source"`

	// Headers are the client request headers, masked.
	Headers map[string][]string `json:"headers,omitempty"`
	// Body is the client request body exactly as received (decompressed).
	Body []byte `json:"body,omitempty"`

	Status          int                 `json:"status"`
	ResponseHeaders map[string][]string `json:"response_headers,omitempty"`
	// Response is the client-facing response body (decompressed). Streaming
	// responses keep their wire framing, e.g. SSE.
	Response []byte `json:"response,omitempty"`
	// Streaming describes response framing for transcript parsing, not client intent.
	Streaming bool `json:"streaming,omitempty"`
	// ResponseTruncated reports that capture dropped part of the response.
	ResponseTruncated bool `json:"response_truncated,omitempty"`

	// WebsocketTimeline is the downstream websocket transcript, when the
	// client used a websocket transport.
	WebsocketTimeline []byte `json:"websocket_timeline,omitempty"`
	// Errors are error messages recorded for the request.
	Errors []string `json:"errors,omitempty"`

	Attempts []Attempt `json:"attempts,omitempty"`
}

// Attempt is one upstream try (retries and fallbacks are separate attempts).
type Attempt struct {
	// Index is 1-based, in the order attempts started.
	Index      int       `json:"index"`
	StartedAt  time.Time `json:"started_at"`
	ResponseAt time.Time `json:"response_at,omitzero"`

	Method string `json:"method"`
	// URL is the upstream URL with sensitive query values masked.
	URL string `json:"url"`
	// UpstreamFormat is an optional hint; the ingester detects it when empty.
	UpstreamFormat string `json:"upstream_format,omitempty"`
	Transport      string `json:"transport,omitempty"`

	Provider  string `json:"provider,omitempty"`
	AuthID    string `json:"auth_id,omitempty"`
	AuthLabel string `json:"auth_label,omitempty"`
	AuthType  string `json:"auth_type,omitempty"`

	// Headers are the upstream request headers, masked.
	Headers map[string][]string `json:"headers,omitempty"`
	// Body is the upstream request body exactly as sent.
	Body []byte `json:"body,omitempty"`

	Status          int                 `json:"status,omitempty"`
	ResponseHeaders map[string][]string `json:"response_headers,omitempty"`
	// Response is the raw upstream response body; streams keep wire framing.
	Response          []byte `json:"response,omitempty"`
	ResponseTruncated bool   `json:"response_truncated,omitempty"`

	WebsocketTimeline []byte   `json:"websocket_timeline,omitempty"`
	Errors            []string `json:"errors,omitempty"`
}
