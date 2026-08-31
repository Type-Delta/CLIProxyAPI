package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// APIKeyEntry is one inbound client API key with optional usage limits.
type APIKeyEntry struct {
	Key             string               `yaml:"key" json:"key"`
	Limits          *KeyLimits           `yaml:"limits,omitempty" json:"limits,omitempty"`
	ExtensionFields map[string]yaml.Node `yaml:"-" json:"-"`
}

// KeyLimits holds optional usage caps for one key.
type KeyLimits struct {
	MaxRequests     int64                `yaml:"max-requests,omitempty" json:"max-requests,omitempty"`
	MaxTokensM      float64              `yaml:"max-tokens-m,omitempty" json:"max-tokens-m,omitempty"`
	Resets          string               `yaml:"resets,omitempty"       json:"resets,omitempty"`
	ExtensionFields map[string]yaml.Node `yaml:"-" json:"-"`
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

// HasExtensionFields reports whether limits carry fields from a newer contract.
func (l KeyLimits) HasExtensionFields() bool {
	return len(l.ExtensionFields) > 0
}

func (l *KeyLimits) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("api key limits must be a mapping")
	}
	decoded := KeyLimits{}
	seen := make(map[string]struct{}, len(node.Content)/2)
	for index := 0; index+1 < len(node.Content); index += 2 {
		name := node.Content[index].Value
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate api key limit field %q", name)
		}
		seen[name] = struct{}{}
		value := node.Content[index+1]
		switch name {
		case "max-requests":
			if err := value.Decode(&decoded.MaxRequests); err != nil {
				return err
			}
		case "max-tokens-m":
			if err := value.Decode(&decoded.MaxTokensM); err != nil {
				return err
			}
		case "resets":
			if err := value.Decode(&decoded.Resets); err != nil {
				return err
			}
		default:
			if decoded.ExtensionFields == nil {
				decoded.ExtensionFields = make(map[string]yaml.Node)
			}
			decoded.ExtensionFields[name] = *cloneAPIKeyYAMLNode(value)
		}
	}
	*l = decoded
	return nil
}

func (l KeyLimits) MarshalYAML() (any, error) {
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if l.MaxRequests != 0 {
		appendAPIKeyYAMLField(node, "max-requests", scalarAPIKeyYAMLNode(l.MaxRequests))
	}
	if l.MaxTokensM != 0 {
		appendAPIKeyYAMLField(node, "max-tokens-m", scalarAPIKeyYAMLNode(l.MaxTokensM))
	}
	if l.Resets != "" {
		appendAPIKeyYAMLField(node, "resets", scalarAPIKeyYAMLNode(l.Resets))
	}
	for _, name := range sortedAPIKeyYAMLFields(l.ExtensionFields, nil) {
		value := l.ExtensionFields[name]
		appendAPIKeyYAMLField(node, name, cloneAPIKeyYAMLNode(&value))
	}
	return node, nil
}

func (l *KeyLimits) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	decoded := KeyLimits{}
	for name, raw := range fields {
		switch name {
		case "max-requests":
			if err := json.Unmarshal(raw, &decoded.MaxRequests); err != nil {
				return err
			}
		case "max-tokens-m":
			if err := json.Unmarshal(raw, &decoded.MaxTokensM); err != nil {
				return err
			}
		case "resets":
			if err := json.Unmarshal(raw, &decoded.Resets); err != nil {
				return err
			}
		default:
			var document yaml.Node
			if err := yaml.Unmarshal(raw, &document); err != nil || len(document.Content) != 1 {
				return fmt.Errorf("decode API key limit extension field %q", name)
			}
			if decoded.ExtensionFields == nil {
				decoded.ExtensionFields = make(map[string]yaml.Node)
			}
			decoded.ExtensionFields[name] = *cloneAPIKeyYAMLNode(document.Content[0])
		}
	}
	*l = decoded
	return nil
}

func (l KeyLimits) MarshalJSON() ([]byte, error) {
	fields := make(map[string]any, 3+len(l.ExtensionFields))
	if l.MaxRequests != 0 {
		fields["max-requests"] = l.MaxRequests
	}
	if l.MaxTokensM != 0 {
		fields["max-tokens-m"] = l.MaxTokensM
	}
	if l.Resets != "" {
		fields["resets"] = l.Resets
	}
	for _, name := range sortedAPIKeyYAMLFields(l.ExtensionFields, nil) {
		value := l.ExtensionFields[name]
		var decoded any
		if err := value.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("encode API key limit extension field %q: %w", name, err)
		}
		fields[name] = decoded
	}
	return json.Marshal(fields)
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
		entry := APIKeyEntry{}
		seen := make(map[string]struct{}, len(node.Content)/2)
		for index := 0; index+1 < len(node.Content); index += 2 {
			name := node.Content[index].Value
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("duplicate api key entry field %q", name)
			}
			seen[name] = struct{}{}
			value := node.Content[index+1]
			switch name {
			case "key":
				if err := value.Decode(&entry.Key); err != nil {
					return err
				}
			case "limits":
				if value.Tag == "!!null" {
					continue
				}
				var limits KeyLimits
				if err := value.Decode(&limits); err != nil {
					return err
				}
				entry.Limits = &limits
			default:
				if entry.ExtensionFields == nil {
					entry.ExtensionFields = make(map[string]yaml.Node)
				}
				entry.ExtensionFields[name] = *cloneAPIKeyYAMLNode(value)
			}
		}
		*e = entry
		return nil
	default:
		return fmt.Errorf("api key entry must be a string or mapping")
	}
}

func (e APIKeyEntry) MarshalYAML() (any, error) {
	if e.hasNoLimits() {
		return e.Key, nil
	}
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	appendAPIKeyYAMLField(node, "key", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: e.Key})
	if e.Limits != nil && (!e.Limits.IsZero() || e.Limits.HasExtensionFields()) {
		var limitsNode yaml.Node
		if err := limitsNode.Encode(e.Limits); err != nil {
			return nil, err
		}
		appendAPIKeyYAMLField(node, "limits", &limitsNode)
	}
	for _, name := range e.extensionFieldNames() {
		value := e.ExtensionFields[name]
		appendAPIKeyYAMLField(node, name, cloneAPIKeyYAMLNode(&value))
	}
	return node, nil
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
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		entry := APIKeyEntry{}
		for name, raw := range fields {
			switch name {
			case "key":
				if err := json.Unmarshal(raw, &entry.Key); err != nil {
					return err
				}
			case "limits":
				if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
					continue
				}
				var limits KeyLimits
				if err := json.Unmarshal(raw, &limits); err != nil {
					return err
				}
				entry.Limits = &limits
			default:
				var document yaml.Node
				if err := yaml.Unmarshal(raw, &document); err != nil || len(document.Content) != 1 {
					return fmt.Errorf("decode api key extension field %q", name)
				}
				if entry.ExtensionFields == nil {
					entry.ExtensionFields = make(map[string]yaml.Node)
				}
				entry.ExtensionFields[name] = *cloneAPIKeyYAMLNode(document.Content[0])
			}
		}
		*e = entry
		return nil
	default:
		return fmt.Errorf("api key entry must be a string or object")
	}
}

func (e APIKeyEntry) MarshalJSON() ([]byte, error) {
	if e.hasNoLimits() {
		return json.Marshal(e.Key)
	}
	fields := make(map[string]any, 2+len(e.ExtensionFields))
	fields["key"] = e.Key
	if e.Limits != nil && (!e.Limits.IsZero() || e.Limits.HasExtensionFields()) {
		fields["limits"] = e.Limits
	}
	for _, name := range e.extensionFieldNames() {
		value := e.ExtensionFields[name]
		var decoded any
		if err := value.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("encode api key extension field %q: %w", name, err)
		}
		fields[name] = decoded
	}
	return json.Marshal(fields)
}

func (e APIKeyEntry) hasNoLimits() bool {
	// A reset cadence without a cap is inert, so serialize it as a bare key.
	return (e.Limits == nil || (e.Limits.IsZero() && !e.Limits.HasExtensionFields())) && len(e.ExtensionFields) == 0
}

// IsStructured reports whether this entry has information that a string-only
// client cannot represent.
func (e APIKeyEntry) IsStructured() bool {
	return (e.Limits != nil && (!e.Limits.IsZero() || e.Limits.HasExtensionFields())) || len(e.ExtensionFields) > 0
}

// APIKeyID returns the stable identifier shared by limits and analytics.
func APIKeyID(raw string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(raw))))
}

// APIKeyConfigRevision returns a secret-free optimistic concurrency token for
// the current ordered key entries.
func APIKeyConfigRevision(entries []APIKeyEntry) string {
	hash := sha256.New()
	for index, entry := range entries {
		_, _ = fmt.Fprintf(hash, "%d\x00%s\x00", index, APIKeyID(entry.Key))
		if entry.Limits != nil {
			encoded, _ := yaml.Marshal(entry.Limits)
			_, _ = hash.Write(encoded)
			_, _ = hash.Write([]byte{0})
		}
		for _, name := range entry.extensionFieldNames() {
			value := entry.ExtensionFields[name]
			encoded, _ := yaml.Marshal(&value)
			_, _ = hash.Write([]byte(name))
			_, _ = hash.Write([]byte{0})
			_, _ = hash.Write(encoded)
			_, _ = hash.Write([]byte{0})
		}
	}
	return fmt.Sprintf("rev-%x", hash.Sum(nil)[:6])
}

// ValidateAPIKeyMutation rejects newly introduced duplicate trimmed keys while
// allowing a legacy duplicate set to load and to be repaired one row at a time.
func ValidateAPIKeyMutation(previous, candidate []APIKeyEntry) error {
	previousCounts := apiKeyCounts(previous)
	candidateCounts := apiKeyCounts(candidate)
	for key, count := range candidateCounts {
		if count <= 1 || count <= previousCounts[key] {
			continue
		}
		return fmt.Errorf("duplicate trimmed API key is not allowed")
	}
	return nil
}

// DuplicateAPIKeyIndexes reports legacy duplicate rows without exposing keys.
func DuplicateAPIKeyIndexes(entries []APIKeyEntry) [][]int {
	byKey := make(map[string][]int)
	order := make([]string, 0, len(entries))
	for index, entry := range entries {
		key := strings.TrimSpace(entry.Key)
		if key == "" {
			continue
		}
		if _, exists := byKey[key]; !exists {
			order = append(order, key)
		}
		byKey[key] = append(byKey[key], index)
	}
	duplicates := make([][]int, 0)
	for _, key := range order {
		if len(byKey[key]) > 1 {
			duplicates = append(duplicates, append([]int(nil), byKey[key]...))
		}
	}
	return duplicates
}

// WeakAPIKeyIndexes returns rows whose trimmed keys are too short for a
// digest-derived identifier to resist offline guessing.
func WeakAPIKeyIndexes(entries []APIKeyEntry) []int {
	indexes := make([]int, 0)
	for index, entry := range entries {
		if IsWeakAPIKey(entry.Key) {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

// IsWeakAPIKey catches short keys and a small set of plainly predictable long
// patterns. It does not return the matching pattern, so callers can warn by
// config index without copying key material into logs or responses.
func IsWeakAPIKey(raw string) bool {
	key := strings.TrimSpace(raw)
	if key == "" {
		return false
	}
	if len(key) < 24 {
		return true
	}
	lower := strings.ToLower(key)
	for _, marker := range []string{"password", "changeme", "letmein", "qwerty", "default", "example", "secretsecret"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	if repeatsAPIKeyUnit(key, 16) {
		return true
	}
	for _, alphabet := range []string{
		"0123456789",
		"abcdefghijklmnopqrstuvwxyz",
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	} {
		if followsAPIKeyAlphabet(key, alphabet, 1) || followsAPIKeyAlphabet(key, alphabet, -1) {
			return true
		}
	}
	return false
}

func repeatsAPIKeyUnit(value string, maxUnit int) bool {
	for unit := 1; unit <= maxUnit && unit*2 <= len(value); unit++ {
		if len(value)%unit != 0 {
			continue
		}
		matched := true
		for index := unit; index < len(value); index++ {
			if value[index] != value[index%unit] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func followsAPIKeyAlphabet(value, alphabet string, direction int) bool {
	if len(value) < 8 || len(alphabet) == 0 {
		return false
	}
	start := strings.IndexByte(alphabet, value[0])
	if start < 0 {
		return false
	}
	for index := 1; index < len(value); index++ {
		position := (start + direction*index) % len(alphabet)
		if position < 0 {
			position += len(alphabet)
		}
		if value[index] != alphabet[position] {
			return false
		}
	}
	return true
}

func apiKeyCounts(entries []APIKeyEntry) map[string]int {
	counts := make(map[string]int, len(entries))
	for _, entry := range entries {
		if key := strings.TrimSpace(entry.Key); key != "" {
			counts[key]++
		}
	}
	return counts
}

func (e APIKeyEntry) extensionFieldNames() []string {
	return sortedAPIKeyYAMLFields(e.ExtensionFields, map[string]struct{}{"key": {}, "limits": {}})
}

func sortedAPIKeyYAMLFields(fields map[string]yaml.Node, excluded map[string]struct{}) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		if _, skip := excluded[name]; !skip {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func scalarAPIKeyYAMLNode(value any) *yaml.Node {
	var node yaml.Node
	_ = node.Encode(value)
	return &node
}

func appendAPIKeyYAMLField(mapping *yaml.Node, name string, value *yaml.Node) {
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name},
		value,
	)
}

func cloneAPIKeyYAMLNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	clone := *node
	clone.Content = make([]*yaml.Node, len(node.Content))
	for index := range node.Content {
		clone.Content[index] = cloneAPIKeyYAMLNode(node.Content[index])
	}
	return &clone
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
	for _, indexes := range DuplicateAPIKeyIndexes(c.APIKeys) {
		log.WithField("config_indexes", indexes).Warn("configured API key appears more than once; usage shares one identity")
	}
	if indexes := WeakAPIKeyIndexes(c.APIKeys); len(indexes) > 0 {
		log.WithField("config_indexes", indexes).Warn("configured API keys are short or use a predictable pattern")
	}
	return nil
}
