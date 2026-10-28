package model

import (
	"testing"
	"time"
)

func TestLegacyEventOutcomes(t *testing.T) {
	sent := time.Now().UTC()
	cases := []struct {
		name, body, outcome, hop string
		status                   int
		dispatched               bool
	}{
		{"codex overload", `{"error":{"code":"server_is_overloaded"}}`, "failed", "provider_to_cpa", 200, true},
		{"codex server", `{"error":{"code":"server_error"}}`, "failed", "provider_to_cpa", 200, true},
		{"kiro", "Kiro stream exception: serviceUnavailableError", "failed", "provider_to_cpa", 200, true},
		{"compat", "upstream stream returned an error payload", "failed", "provider_to_cpa", 200, true},
		{"incomplete", "upstream stream closed before [DONE]", "failed", "provider_to_cpa", 200, true},
		{"peer", "stream error: stream ID 11; INTERNAL_ERROR; received from peer", "failed", "provider_to_cpa", 200, true},
		{"local guard", "Claude OAuth safeguard flagged this request", "failed", "cpa_to_provider", 403, false},
		{"canceled", "context canceled", "canceled", "", 499, true},
		{"url canceled", `Post "https://provider.invalid/v1": context canceled`, "canceled", "", 499, true},
		{"body closed remains unknown", "http2: response body closed", "failed", "", 200, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Event{UpstreamErrorBody: &tc.body, UpstreamStatusCode: &tc.status}
			if tc.dispatched {
				e.UpstreamSentAt = &sent
			}
			final := "context canceled"
			e.ProxyError = &final
			ClassifyEvent(&e)
			hop := ""
			if e.FailureHop != nil {
				hop = *e.FailureHop
			}
			if e.Outcome != tc.outcome || hop != tc.hop || e.Succeeded != (tc.outcome != "failed") {
				t.Fatalf("classification: %+v hop=%s", e, hop)
			}
			if tc.outcome == "canceled" && (e.CancellationSource == nil || *e.CancellationSource != "unknown" || e.Completed()) {
				t.Fatalf("cancellation=%+v", e)
			}
			if tc.name == "local guard" && e.UpstreamStatusCode != nil {
				t.Fatal("invented upstream status")
			}
		})
	}
}
