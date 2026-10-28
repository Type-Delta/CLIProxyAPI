package model

import (
	"encoding/json"
	"os"
	"testing"
)

// Optional audit replay keeps production diagnostics outside the repository.
func TestReplaySavedEventOutcomes(t *testing.T) {
	path := os.Getenv("CPAUK_REPLAY_PATH")
	if path == "" {
		t.Skip("set CPAUK_REPLAY_PATH to an external saved event array")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	if err = json.Unmarshal(data, &events); err != nil {
		t.Fatal(err)
	}
	provider, canceled, wrong := 0, 0, 0
	for _, e := range events {
		body := ""
		if e.UpstreamErrorBody != nil {
			body = *e.UpstreamErrorBody
		}
		expected := ProviderFailureBody(body)
		ClassifyEvent(&e)
		if expected {
			provider++
			if e.FailureHop == nil || *e.FailureHop != "provider_to_cpa" {
				wrong++
			}
		}
		if e.Outcome == "canceled" {
			canceled++
		}
	}
	t.Logf("events=%d provider_failures=%d wrong_hops=%d historical_canceled=%d", len(events), provider, wrong, canceled)
	if wrong != 0 {
		t.Fail()
	}
}
