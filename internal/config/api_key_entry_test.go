package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAPIKeyEntryYAML(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		want     APIKeyEntry
		wantBare bool
	}{
		{
			name:     "bare string",
			input:    "plain-key",
			want:     APIKeyEntry{Key: "plain-key"},
			wantBare: true,
		},
		{
			name: "mapping with limits",
			input: `key: limited-key
limits:
  max-requests: 1000
  max-tokens-m: 0.5
  resets: weekly`,
			want: APIKeyEntry{Key: "limited-key", Limits: &KeyLimits{
				MaxRequests: 1000,
				MaxTokensM:  0.5,
				Resets:      "weekly",
			}},
		},
		{
			name:     "mapping without limits",
			input:    "key: plain-key",
			want:     APIKeyEntry{Key: "plain-key"},
			wantBare: true,
		},
		{
			name: "mapping with zero limits",
			input: `key: plain-key
limits: {}`,
			want:     APIKeyEntry{Key: "plain-key", Limits: &KeyLimits{}},
			wantBare: true,
		},
		{
			name: "mapping with resets only",
			input: `key: plain-key
limits:
  resets: daily`,
			want:     APIKeyEntry{Key: "plain-key", Limits: &KeyLimits{Resets: "daily"}},
			wantBare: true,
		},
		{
			name:     "bare string round trip",
			input:    "round-trip-key",
			want:     APIKeyEntry{Key: "round-trip-key"},
			wantBare: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var entry APIKeyEntry
			if errUnmarshal := yaml.Unmarshal([]byte(test.input), &entry); errUnmarshal != nil {
				t.Fatalf("yaml.Unmarshal() error = %v", errUnmarshal)
			}
			if !reflect.DeepEqual(entry, test.want) {
				t.Fatalf("yaml.Unmarshal() = %#v, want %#v", entry, test.want)
			}

			data, errMarshal := yaml.Marshal(entry)
			if errMarshal != nil {
				t.Fatalf("yaml.Marshal() error = %v", errMarshal)
			}
			if test.wantBare {
				assertYAMLBareString(t, data)
			}
		})
	}
}

func TestAPIKeyEntryJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		want     APIKeyEntry
		wantBare bool
	}{
		{
			name:     "bare string",
			input:    `"plain-key"`,
			want:     APIKeyEntry{Key: "plain-key"},
			wantBare: true,
		},
		{
			name:  "mapping with limits",
			input: `{"key":"limited-key","limits":{"max-requests":1000,"max-tokens-m":0.5,"resets":"weekly"}}`,
			want: APIKeyEntry{Key: "limited-key", Limits: &KeyLimits{
				MaxRequests: 1000,
				MaxTokensM:  0.5,
				Resets:      "weekly",
			}},
		},
		{
			name:     "mapping without limits",
			input:    `{"key":"plain-key"}`,
			want:     APIKeyEntry{Key: "plain-key"},
			wantBare: true,
		},
		{
			name:     "mapping with zero limits",
			input:    `{"key":"plain-key","limits":{}}`,
			want:     APIKeyEntry{Key: "plain-key", Limits: &KeyLimits{}},
			wantBare: true,
		},
		{
			name:     "mapping with resets only",
			input:    `{"key":"plain-key","limits":{"resets":"daily"}}`,
			want:     APIKeyEntry{Key: "plain-key", Limits: &KeyLimits{Resets: "daily"}},
			wantBare: true,
		},
		{
			name:     "bare string round trip",
			input:    `"round-trip-key"`,
			want:     APIKeyEntry{Key: "round-trip-key"},
			wantBare: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var entry APIKeyEntry
			if errUnmarshal := json.Unmarshal([]byte(test.input), &entry); errUnmarshal != nil {
				t.Fatalf("json.Unmarshal() error = %v", errUnmarshal)
			}
			if !reflect.DeepEqual(entry, test.want) {
				t.Fatalf("json.Unmarshal() = %#v, want %#v", entry, test.want)
			}

			data, errMarshal := json.Marshal(entry)
			if errMarshal != nil {
				t.Fatalf("json.Marshal() error = %v", errMarshal)
			}
			if test.wantBare {
				assertJSONBareString(t, data)
			}
		})
	}
}

func TestKeyLimitsMaxTokens(t *testing.T) {
	tests := []struct {
		name       string
		maxTokensM float64
		want       int64
	}{
		{name: "whole millions", maxTokensM: 20, want: 20_000_000},
		{name: "half million", maxTokensM: 0.5, want: 500_000},
		{name: "zero", maxTokensM: 0, want: 0},
		{name: "negative", maxTokensM: -1, want: 0},
		{name: "rounds to nearest token", maxTokensM: 1.2345678, want: 1_234_568},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := (KeyLimits{MaxTokensM: test.maxTokensM}).MaxTokens(); got != test.want {
				t.Fatalf("MaxTokens() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestKeyLimitsIsZero(t *testing.T) {
	tests := []struct {
		name   string
		limits *KeyLimits
		want   bool
	}{
		{name: "nil", want: true},
		{name: "empty", limits: &KeyLimits{}, want: true},
		{name: "requests set", limits: &KeyLimits{MaxRequests: 1}, want: false},
		{name: "tokens set", limits: &KeyLimits{MaxTokensM: 0.5}, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.limits == nil || test.limits.IsZero()
			if got != test.want {
				t.Fatalf("IsZero() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestSDKConfigAPIKeyHelpers(t *testing.T) {
	tests := []struct {
		name       string
		config     *SDKConfig
		wantKeys   []string
		wantLimits map[string]KeyLimits
	}{
		{name: "nil config"},
		{
			name: "trims keys and omits unlimited limits",
			config: &SDKConfig{APIKeys: []APIKeyEntry{
				{Key: " first "},
				{Key: ""},
				{Key: " \t "},
				{Key: "second"},
				{Key: " limited ", Limits: &KeyLimits{MaxRequests: 10, Resets: "daily"}},
				{Key: "unlimited", Limits: &KeyLimits{}},
				{Key: "reset-only", Limits: &KeyLimits{Resets: "weekly"}},
				{Key: "", Limits: &KeyLimits{MaxTokensM: 20}},
			}},
			wantKeys: []string{"first", "second", "limited", "unlimited", "reset-only"},
			wantLimits: map[string]KeyLimits{
				"limited": {MaxRequests: 10, Resets: "daily"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.config.APIKeyStrings(); !reflect.DeepEqual(got, test.wantKeys) {
				t.Fatalf("APIKeyStrings() = %#v, want %#v", got, test.wantKeys)
			}
			if got := test.config.APIKeyLimits(); !reflect.DeepEqual(got, test.wantLimits) {
				t.Fatalf("APIKeyLimits() = %#v, want %#v", got, test.wantLimits)
			}
		})
	}
}

func TestAPIKeyLimitValidationOnConfigLoad(t *testing.T) {
	tests := []struct {
		name           string
		limits         string
		wantErr        string
		wantEntryIndex bool
	}{
		{name: "hourly resets", limits: "resets: hourly"},
		{name: "daily resets", limits: "resets: daily"},
		{name: "weekly resets", limits: "resets: weekly"},
		{name: "monthly resets", limits: "resets: monthly"},
		{name: "empty resets", limits: `resets: ""`},
		{name: "omitted resets", limits: "max-requests: 1"},
		{name: "unknown resets with cap", limits: "max-requests: 1\n      resets: yearly", wantErr: "invalid resets", wantEntryIndex: true},
		{name: "negative requests", limits: "max-requests: -1", wantErr: "max-requests", wantEntryIndex: true},
		{name: "negative tokens", limits: "max-tokens-m: -0.5", wantErr: "max-tokens-m", wantEntryIndex: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := []byte("api-keys:\n  - plain-key\n  - key: secret-key-must-not-appear\n    limits:\n      " + test.limits + "\n")
			path := filepath.Join(t.TempDir(), "config.yaml")
			if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
				t.Fatalf("os.WriteFile() error = %v", errWrite)
			}

			_, errLoad := LoadConfig(path)
			if test.wantErr == "" {
				if errLoad != nil {
					t.Fatalf("LoadConfig() error = %v", errLoad)
				}
				return
			}
			if errLoad == nil {
				t.Fatal("LoadConfig() error = nil, want validation error")
			}
			if !strings.Contains(errLoad.Error(), test.wantErr) {
				t.Fatalf("LoadConfig() error = %q, want %q", errLoad, test.wantErr)
			}
			if test.wantEntryIndex && !strings.Contains(errLoad.Error(), "index 1") {
				t.Fatalf("LoadConfig() error = %q, want entry index", errLoad)
			}
			if strings.Contains(errLoad.Error(), "secret-key-must-not-appear") {
				t.Fatalf("LoadConfig() error leaked API key: %q", errLoad)
			}
		})
	}
}

func assertYAMLBareString(t *testing.T, data []byte) {
	t.Helper()
	var node yaml.Node
	if errUnmarshal := yaml.Unmarshal(data, &node); errUnmarshal != nil {
		t.Fatalf("yaml.Unmarshal(marshaled) error = %v", errUnmarshal)
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.ScalarNode {
		t.Fatalf("yaml.Marshal() = %q, want bare string", data)
	}
}

func assertJSONBareString(t *testing.T, data []byte) {
	t.Helper()
	if len(data) == 0 || data[0] != '"' {
		t.Fatalf("json.Marshal() = %s, want bare string", data)
	}
}
