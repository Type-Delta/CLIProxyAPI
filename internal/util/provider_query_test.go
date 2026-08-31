package util

import (
	"strings"
	"testing"
)

func TestMaskSensitiveQueryMasksAnalyticsKeyIdentities(t *testing.T) {
	fullID := strings.Repeat("a", 64)
	masked := MaskSensitiveQuery("start=2026-08-01&key_id=" + fullID + "&key_ids%5B%5D=" + fullID)
	if strings.Contains(masked, fullID) || !strings.Contains(masked, "key_id=") {
		t.Fatalf("analytics key identity was not masked: %q", masked)
	}
}
