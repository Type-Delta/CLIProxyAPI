package usage

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBoundRawUsagePreservesTotalsAfterOversizedMetadata(t *testing.T) {
	raw := `{"response":{"extra":"` + strings.Repeat("界", 20000) + `","usage":{"input_tokens":123,"output_tokens_details":{"reasoning_tokens":45}},"status":"completed"}}`
	got := BoundRawUsage(raw)
	if len(got) > 40960 || !json.Valid([]byte(got)) || !strings.Contains(got, `"input_tokens":123`) || !strings.Contains(got, `"reasoning_tokens":45`) {
		t.Fatalf("lost aggregate usage or invalid JSON: %.200s", got)
	}
}

func TestBoundRawUsageBoundaryAndEscaping(t *testing.T) {
	for _, payload := range []string{strings.Repeat("x", 40930), strings.Repeat("<&界", 15000)} {
		got := BoundRawUsage(`{"_truncated":false,"usage":{"total_tokens":42},"extra":"` + payload + `"}`)
		if len(got) > 40960 || !json.Valid([]byte(got)) || !strings.Contains(got, `"total_tokens":42`) {
			t.Fatalf("invalid bounded diagnostics: %d", len(got))
		}
		if len(payload) > 40960 && !strings.Contains(got, `"_truncated":true`) {
			t.Fatal("overflow marker overwritten")
		}
	}
}

func TestBoundRawUsagePrioritizesTotalsOverLargeBreakdowns(t *testing.T) {
	raw := `{"usage":{"input_tokens":1,"input_tokens_details":{"extra":"` + strings.Repeat("x", 40844) + `"},"output_tokens":2,"total_tokens":3},"metadata":"` + strings.Repeat("y", 1000) + `"}`
	got := BoundRawUsage(raw)
	for _, counter := range []string{`"input_tokens":1`, `"output_tokens":2`, `"total_tokens":3`} {
		if !strings.Contains(got, counter) {
			t.Fatalf("missing aggregate counter %s", counter)
		}
	}
	if !json.Valid([]byte(got)) || len(got) > MaxRawUsageBytes {
		t.Fatal("invalid bounded diagnostics")
	}
}
