package config

import (
	"math"
	"testing"
)

// TestMaxTokensRejectsNonFiniteAndOverflow guards a silent limit bypass:
// converting a non-finite or out-of-range float64 to int64 is
// implementation-defined and yields MinInt64 on amd64, which the gate reads back
// as "no cap" — turning a configured token limit into unlimited.
func TestMaxTokensRejectsNonFiniteAndOverflow(t *testing.T) {
	tests := []struct {
		name  string
		value float64
		want  int64
	}{
		{name: "twenty million", value: 20, want: 20_000_000},
		{name: "half million", value: 0.5, want: 500_000},
		{name: "rounds to nearest", value: 1.0000005, want: 1_000_001},
		{name: "zero is unlimited", value: 0, want: 0},
		{name: "negative is unlimited", value: -5, want: 0},
		{name: "NaN is unlimited", value: math.NaN(), want: 0},
		{name: "positive infinity saturates", value: math.Inf(1), want: math.MaxInt64},
		{name: "negative infinity is unlimited", value: math.Inf(-1), want: 0},
		{name: "overflowing magnitude saturates", value: 1e19, want: math.MaxInt64},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := KeyLimits{MaxTokensM: test.value}.MaxTokens()
			if got != test.want {
				t.Fatalf("MaxTokens() = %d, want %d", got, test.want)
			}
			if got < 0 {
				t.Fatalf("MaxTokens() = %d; a negative cap disables the limit", got)
			}
		})
	}
}

// TestValidateAPIKeyLimitsRejectsUnusableTokenCaps ensures operators get an error
// instead of a silently saturated or disabled cap.
func TestValidateAPIKeyLimitsRejectsUnusableTokenCaps(t *testing.T) {
	tests := []struct {
		name    string
		limits  KeyLimits
		wantErr bool
	}{
		{name: "valid", limits: KeyLimits{MaxTokensM: 20, Resets: "weekly"}, wantErr: false},
		{name: "valid fractional", limits: KeyLimits{MaxTokensM: 0.5}, wantErr: false},
		{name: "NaN", limits: KeyLimits{MaxTokensM: math.NaN()}, wantErr: true},
		{name: "infinity", limits: KeyLimits{MaxTokensM: math.Inf(1)}, wantErr: true},
		{name: "negative", limits: KeyLimits{MaxTokensM: -1}, wantErr: true},
		{name: "too large", limits: KeyLimits{MaxTokensM: 1e19}, wantErr: true},
		{name: "negative requests", limits: KeyLimits{MaxRequests: -1}, wantErr: true},
		{name: "bad cadence", limits: KeyLimits{MaxRequests: 1, Resets: "fortnightly"}, wantErr: true},
	}

	const secret = "super-secret-key"
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limits := test.limits
			cfg := &SDKConfig{APIKeys: []APIKeyEntry{{Key: secret, Limits: &limits}}}

			errValidate := cfg.ValidateAPIKeyLimits()
			if test.wantErr && errValidate == nil {
				t.Fatal("ValidateAPIKeyLimits() = nil, want an error")
			}
			if !test.wantErr && errValidate != nil {
				t.Fatalf("ValidateAPIKeyLimits() = %v, want nil", errValidate)
			}
			// The error identifies the entry positionally and must never echo the key.
			if errValidate != nil && contains(errValidate.Error(), secret) {
				t.Fatalf("validation error leaked the API key: %v", errValidate)
			}
		})
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
