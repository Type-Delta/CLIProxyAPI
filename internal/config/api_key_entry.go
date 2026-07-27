package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"gopkg.in/yaml.v3"
)

// APIKeyEntry is one inbound client API key with optional usage limits.
type APIKeyEntry struct {
	Key    string     `yaml:"key" json:"key"`
	Limits *KeyLimits `yaml:"limits,omitempty" json:"limits,omitempty"`
}

// KeyLimits holds optional usage caps for one key.
type KeyLimits struct {
	MaxRequests int64   `yaml:"max-requests,omitempty" json:"max-requests,omitempty"`
	MaxTokensM  float64 `yaml:"max-tokens-m,omitempty" json:"max-tokens-m,omitempty"`
	Resets      string  `yaml:"resets,omitempty"       json:"resets,omitempty"`
}

// MaxTokens resolves the configured million-token limit to an absolute count.
// maxTokensMCeiling is the largest max-tokens-m that still fits in an int64
// once converted to absolute tokens.
const maxTokensMCeiling = float64(math.MaxInt64) / 1_000_000

func (l KeyLimits) MaxTokens() int64 {
	// NaN comparisons are always false, so reject it explicitly: converting a
	// non-finite float to int64 is implementation-defined and yields MinInt64 on
	// amd64, which would read back as "no cap" and silently disable the limit.
	if math.IsNaN(l.MaxTokensM) || l.MaxTokensM <= 0 {
		return 0
	}
	if math.IsInf(l.MaxTokensM, 1) || l.MaxTokensM >= maxTokensMCeiling {
		// Saturate rather than overflow. An enormous cap is effectively no cap,
		// but it fails in the safe direction instead of wrapping negative.
		return math.MaxInt64
	}
	return int64(math.Round(l.MaxTokensM * 1_000_000))
}

// IsZero reports whether neither usage cap is configured.
func (l KeyLimits) IsZero() bool {
	return l.MaxRequests == 0 && l.MaxTokensM == 0
}

func (e *APIKeyEntry) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!str" {
			return fmt.Errorf("api key entry must be a string or mapping")
		}
		*e = APIKeyEntry{Key: node.Value}
		return nil
	case yaml.MappingNode:
		type rawAPIKeyEntry APIKeyEntry
		var entry rawAPIKeyEntry
		if err := node.Decode(&entry); err != nil {
			return err
		}
		*e = APIKeyEntry(entry)
		return nil
	default:
		return fmt.Errorf("api key entry must be a string or mapping")
	}
}

func (e APIKeyEntry) MarshalYAML() (any, error) {
	if e.Limits == nil || e.Limits.IsZero() {
		return e.Key, nil
	}
	type rawAPIKeyEntry APIKeyEntry
	return rawAPIKeyEntry(e), nil
}

func (e *APIKeyEntry) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return fmt.Errorf("api key entry must be a string or object")
	}

	switch data[0] {
	case '"':
		var key string
		if err := json.Unmarshal(data, &key); err != nil {
			return err
		}
		*e = APIKeyEntry{Key: key}
		return nil
	case '{':
		type rawAPIKeyEntry APIKeyEntry
		var entry rawAPIKeyEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			return err
		}
		*e = APIKeyEntry(entry)
		return nil
	default:
		return fmt.Errorf("api key entry must be a string or object")
	}
}

func (e APIKeyEntry) MarshalJSON() ([]byte, error) {
	if e.Limits == nil || e.Limits.IsZero() {
		return json.Marshal(e.Key)
	}
	type rawAPIKeyEntry APIKeyEntry
	return json.Marshal(rawAPIKeyEntry(e))
}

// APIKeyStrings returns just the key strings, in order, trimmed, skipping empties.
func (c *SDKConfig) APIKeyStrings() []string {
	if c == nil {
		return nil
	}
	keys := make([]string, 0, len(c.APIKeys))
	for _, entry := range c.APIKeys {
		if key := strings.TrimSpace(entry.Key); key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

// APIKeyLimits returns key -> limits for keys that have any limit configured.
func (c *SDKConfig) APIKeyLimits() map[string]KeyLimits {
	if c == nil {
		return nil
	}
	limits := make(map[string]KeyLimits)
	for _, entry := range c.APIKeys {
		key := strings.TrimSpace(entry.Key)
		if key == "" || entry.Limits == nil || entry.Limits.IsZero() {
			continue
		}
		limits[key] = *entry.Limits
	}
	return limits
}

// ValidateAPIKeyLimits validates configured per-key usage limits.
func (c *SDKConfig) ValidateAPIKeyLimits() error {
	if c == nil {
		return nil
	}
	for index, entry := range c.APIKeys {
		if entry.Limits == nil {
			continue
		}
		if entry.Limits.MaxRequests < 0 {
			return fmt.Errorf("api key entry at index %d: max-requests must not be negative", index)
		}
		if math.IsNaN(entry.Limits.MaxTokensM) || math.IsInf(entry.Limits.MaxTokensM, 0) {
			return fmt.Errorf("api key entry at index %d: max-tokens-m must be a finite number", index)
		}
		if entry.Limits.MaxTokensM < 0 {
			return fmt.Errorf("api key entry at index %d: max-tokens-m must not be negative", index)
		}
		if entry.Limits.MaxTokensM >= maxTokensMCeiling {
			return fmt.Errorf("api key entry at index %d: max-tokens-m is too large", index)
		}
		switch strings.TrimSpace(entry.Limits.Resets) {
		case "", "hourly", "daily", "weekly", "monthly":
		default:
			return fmt.Errorf("api key entry at index %d: invalid resets value; must be hourly, daily, weekly, monthly, or empty", index)
		}
	}
	return nil
}
