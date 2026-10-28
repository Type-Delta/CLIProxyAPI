package model

import "strings"

// LegacyCanceledAttempt accepts only attempt-level cancellation evidence. A final
// proxy error belongs to the request and may follow genuine failed retry attempts.
func LegacyCanceledAttempt(body string) bool {
	body = strings.TrimSpace(body)
	return body == "context canceled" || ((strings.HasPrefix(body, "Post \"") || strings.HasPrefix(body, "Get \"")) && strings.HasSuffix(body, "\": context canceled"))
}

func ProviderFailureBody(body string) bool {
	body = strings.ToLower(body)
	for _, marker := range []string{"server_is_overloaded", "\"code\":\"server_error\"", "\"type\":\"server_error\"", "kiro stream exception", "upstream stream returned an error payload", "stream disconnected before completion", "upstream stream closed before", "unexpected eof", "internal_error; received from peer"} {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

// ClassifyEvent supplies historical metadata without rewriting stored rows or
// aggregates. Explicit outcomes always win over request-level diagnostics.
func ClassifyEvent(e *Event) {
	legacy := e.Outcome == ""
	body := ""
	if e.UpstreamErrorBody != nil {
		body = *e.UpstreamErrorBody
	}
	if legacy {
		e.Outcome = "failed"
		if e.Succeeded {
			e.Outcome = "succeeded"
		} else if LegacyCanceledAttempt(body) {
			e.Outcome = "canceled"
			source := "unknown"
			e.CancellationSource = &source
		}
	}
	e.Succeeded = e.Outcome != "failed"
	if e.Outcome != "failed" {
		e.FailureHop = nil
		e.ErrorClass = nil
		return
	}
	if e.FailureHop != nil {
		return
	}
	hop := ""
	if e.UpstreamSentAt == nil && (e.ExecutorType == "auth-selection" || strings.HasPrefix(body, "Claude OAuth safeguard")) {
		hop = "cpa_to_provider"
		e.UpstreamStatusCode = nil
	} else if ProviderFailureBody(body) || (e.UpstreamStatusCode != nil && *e.UpstreamStatusCode >= 400 && *e.UpstreamStatusCode != 499) {
		hop = "provider_to_cpa"
	} else if e.UpstreamSentAt != nil && e.ProxyError != nil && !strings.Contains(body, "context canceled") && body == "" {
		hop = "cpa_to_client"
	}
	if hop != "" {
		e.FailureHop = &hop
	}
}

// Completed excludes partial canceled output from completion and latency samples.
func (e Event) Completed() bool { return e.Succeeded && e.Outcome != "canceled" }

// EventOutcomeSQL mirrors the narrow legacy branch above for filtering before
// pagination. Request-level proxy errors deliberately do not participate.
const EventOutcomeSQL = `COALESCE(outcome, CASE WHEN succeeded = 1 THEN 'succeeded'
WHEN trim(upstream_error_body) = 'context canceled'
OR trim(upstream_error_body) GLOB 'Post "*": context canceled'
OR trim(upstream_error_body) GLOB 'Get "*": context canceled' THEN 'canceled' ELSE 'failed' END)`
