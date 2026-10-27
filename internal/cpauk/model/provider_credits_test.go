package model

import (
	"strings"
	"testing"
)

func TestValidProviderCredits(t *testing.T) {
	for _, value := range []string{"0", "1", "0.000000000000000001", strings.Repeat("9", 64), "123.456"} {
		if !ValidProviderCredits(value) {
			t.Errorf("rejected %q", value)
		}
	}
	for _, value := range []string{"", "01", "1.0", "1.", ".1", "-1", "+1", "1e3", "NaN", strings.Repeat("9", 65), "0." + strings.Repeat("1", 19)} {
		if ValidProviderCredits(value) {
			t.Errorf("accepted %q", value)
		}
	}
}
