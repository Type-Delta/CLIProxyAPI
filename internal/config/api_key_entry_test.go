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

func TestAPIKeyEntryPreservesUnknownStructuredFields(t *testing.T) {
	yamlInput := `key: tenant-key
label: tenant-a
limits:
  max-requests: 10
  burst: 3
metadata:
  owner: operations
`
	var fromYAML APIKeyEntry
	if errUnmarshal := yaml.Unmarshal([]byte(yamlInput), &fromYAML); errUnmarshal != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", errUnmarshal)
	}
	if !fromYAML.IsStructured() || len(fromYAML.ExtensionFields) != 2 || fromYAML.Limits == nil || !fromYAML.Limits.HasExtensionFields() {
		t.Fatalf("decoded entry did not retain extension fields: %#v", fromYAML)
	}

	jsonData, errJSON := json.Marshal(fromYAML)
	if errJSON != nil {
		t.Fatalf("json.Marshal() error = %v", errJSON)
	}
	for _, field := range []string{`"label":"tenant-a"`, `"burst":3`, `"metadata"`} {
		if !strings.Contains(string(jsonData), field) {
			t.Fatalf("JSON = %s, want %s", jsonData, field)
		}
	}

	var fromJSON APIKeyEntry
	if errDecode := json.Unmarshal(jsonData, &fromJSON); errDecode != nil {
		t.Fatalf("json.Unmarshal() error = %v", errDecode)
	}
	yamlData, errYAML := yaml.Marshal(fromJSON)
	if errYAML != nil {
		t.Fatalf("yaml.Marshal() error = %v", errYAML)
	}
	var roundTrip map[string]any
	if errDecode := yaml.Unmarshal(yamlData, &roundTrip); errDecode != nil {
		t.Fatalf("decode round-trip YAML: %v", errDecode)
	}
	if roundTrip["label"] != "tenant-a" {
		t.Fatalf("round-trip label = %#v", roundTrip["label"])
	}
	limits, ok := roundTrip["limits"].(map[string]any)
	if !ok || limits["burst"] != 3 {
		t.Fatalf("round-trip limits = %#v", roundTrip["limits"])
	}
	metadata, ok := roundTrip["metadata"].(map[string]any)
	if !ok || metadata["owner"] != "operations" {
		t.Fatalf("round-trip metadata = %#v", roundTrip["metadata"])
	}
}

func TestAPIKeyIdentityAndDuplicateMutation(t *testing.T) {
	if got, want := APIKeyID("  tenant-key  "), APIKeyID("tenant-key"); got != want {
		t.Fatalf("trimmed APIKeyID = %q, want %q", got, want)
	}
	if got := APIKeyID("tenant-key"); len(got) != 64 {
		t.Fatalf("APIKeyID length = %d, want 64", len(got))
	}

	legacy := []APIKeyEntry{{Key: " duplicate "}, {Key: "duplicate"}}
	if errMutation := ValidateAPIKeyMutation(legacy, legacy); errMutation != nil {
		t.Fatalf("legacy duplicate rejected: %v", errMutation)
	}
	if got := DuplicateAPIKeyIndexes(legacy); !reflect.DeepEqual(got, [][]int{{0, 1}}) {
		t.Fatalf("DuplicateAPIKeyIndexes() = %#v", got)
	}
	if errMutation := ValidateAPIKeyMutation(nil, []APIKeyEntry{{Key: "new"}, {Key: " new "}}); errMutation == nil {
		t.Fatal("new duplicate accepted")
	}
	if errMutation := ValidateAPIKeyMutation(legacy, []APIKeyEntry{{Key: "duplicate"}}); errMutation != nil {
		t.Fatalf("repairing duplicate rejected: %v", errMutation)
	}
}

func TestIsWeakAPIKeyDetectsPredictableLongValues(t *testing.T) {
	tests := []struct {
		name string
		key  string
		weak bool
	}{
		{name: "empty is not configured", key: ""},
		{name: "short", key: "short-key", weak: true},
		{name: "repeated character", key: strings.Repeat("x", 40), weak: true},
		{name: "repeated unit", key: "abc123abc123abc123abc123abc123", weak: true},
		{name: "ascending sequence", key: "012345678901234567890123456789", weak: true},
		{name: "descending sequence", key: "zyxwvutsrqponmlkjihgfedcbazyxw", weak: true},
		{name: "dictionary marker", key: "company-password-production-2026", weak: true},
		{name: "mixed high entropy", key: "aZ9_kP3vN7qR2mX8cL5tH1sW6jD4", weak: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsWeakAPIKey(test.key); got != test.weak {
				t.Fatalf("IsWeakAPIKey() = %t, want %t", got, test.weak)
			}
		})
	}
}

func TestAPIKeyConfigRevisionChangesWithStructuredFields(t *testing.T) {
	base := []APIKeyEntry{{Key: "key", Limits: &KeyLimits{MaxRequests: 1}}}
	copyEntries := []APIKeyEntry{{Key: " key ", Limits: &KeyLimits{MaxRequests: 1}}}
	if got, want := APIKeyConfigRevision(copyEntries), APIKeyConfigRevision(base); got != want {
		t.Fatalf("whitespace-only key revision changed: %q != %q", got, want)
	}
	copyEntries[0].Limits.MaxRequests = 2
	if APIKeyConfigRevision(copyEntries) == APIKeyConfigRevision(base) {
		t.Fatal("limit edit did not change revision")
	}
}

func TestSaveConfigPreservesAPIKeyExtensionFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	input := []byte(`api-keys:
  - key: tenant-key-with-enough-entropy
    label: tenant-a
    limits:
      max-requests: 10
      future-window: quarter
`)
	if errWrite := os.WriteFile(path, input, 0o600); errWrite != nil {
		t.Fatalf("write fixture: %v", errWrite)
	}
	cfg, errLoad := LoadConfig(path)
	if errLoad != nil {
		t.Fatalf("LoadConfig() error = %v", errLoad)
	}
	cfg.APIKeys[0].Limits.MaxRequests = 20
	if errSave := SaveConfigPreserveComments(path, cfg); errSave != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", errSave)
	}

	reloaded, errReload := LoadConfig(path)
	if errReload != nil {
		t.Fatalf("reload error = %v", errReload)
	}
	entry := reloaded.APIKeys[0]
	if entry.Limits == nil || entry.Limits.MaxRequests != 20 || !entry.Limits.HasExtensionFields() || len(entry.ExtensionFields) != 1 {
		t.Fatalf("reloaded entry = %#v", entry)
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
