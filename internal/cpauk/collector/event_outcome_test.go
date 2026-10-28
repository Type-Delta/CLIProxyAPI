package collector

import (
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"testing"
	"time"
)

func TestTerminalFailureSeparateFromHTTPTransport(t *testing.T) {
	for _, body := range []string{`{"error":{"code":"server_is_overloaded"}}`, "Kiro stream exception: error", "upstream stream returned an error payload", "upstream stream closed before [DONE]", "stream error: INTERNAL_ERROR; received from peer"} {
		record := validRecord()
		record.Failed = true
		record.Outcome = "failed"
		record.FailureHop = "provider_to_cpa"
		record.UpstreamStatusCode = 200
		record.UpstreamSentAt = time.Now().UTC()
		record.Fail = coreusage.Failure{StatusCode: 502, Body: body}
		got, err := newHopSanitizer().Sanitize(adaptRecord(record))
		if err != nil {
			t.Fatal(err)
		}
		if got.Event.UpstreamStatusCode == nil || *got.Event.UpstreamStatusCode != 200 || got.Event.FailureHop == nil || *got.Event.FailureHop != "provider_to_cpa" || got.Event.ErrorClass == nil || *got.Event.ErrorClass == "transport_failure" {
			t.Fatalf("event=%+v", got.Event)
		}
	}
	record := validRecord()
	record.Failed = true
	record.Outcome = "failed"
	record.FailureHop = "cpa_to_provider"
	record.Fail = coreusage.Failure{StatusCode: 403, Body: "Claude OAuth safeguard flagged this request"}
	got, err := newHopSanitizer().Sanitize(adaptRecord(record))
	if err != nil {
		t.Fatal(err)
	}
	if got.Event.UpstreamStatusCode != nil || got.Event.FailureHop == nil || *got.Event.FailureHop != "cpa_to_provider" {
		t.Fatalf("local rejection=%+v", got.Event)
	}
	record.Outcome = "canceled"
	record.CancellationSource = "client"
	record.FailureHop = ""
	got, err = newHopSanitizer().Sanitize(adaptRecord(record))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Event.Succeeded || got.Event.FailureHop != nil || got.Event.ErrorClass != nil {
		t.Fatalf("canceled=%+v", got.Event)
	}
}
