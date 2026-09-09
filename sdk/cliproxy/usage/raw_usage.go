package usage

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// BoundRawUsage bounds generation diagnostics without cutting valid JSON values.
// On overflow aggregate usage takes precedence over provider metadata.
func BoundRawUsage(raw string) string {
	var value any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if !json.Valid([]byte(raw)) || decoder.Decode(&value) != nil {
		return truncateUTF8(raw, MaxRawUsageBytes)
	}
	var compact, escaped bytes.Buffer
	_ = json.Compact(&compact, []byte(raw))
	json.HTMLEscape(&escaped, compact.Bytes())
	encoded := escaped.Bytes()
	if len(encoded) <= MaxRawUsageBytes {
		return string(encoded)
	}
	result := map[string]any{"_truncated": true}
	var collect func(map[string]any, map[string]any)
	collect = func(source, target map[string]any) {
		keys := make([]string, 0, len(source))
		for key := range source {
			keys = append(keys, key)
		}
		priority := func(key string) int {
			if strings.Contains(strings.ToLower(key), "token") {
				if _, scalar := source[key].(json.Number); scalar {
					return 0
				}
				return 2
			}
			switch key {
			case "usage", "usageMetadata", "usage_metadata", "response", "message":
				return 0
			case "model", "status", "error", "created_at", "completed_at", "service_tier", "reasoning":
				return 1
			}
			return 2
		}
		sort.Slice(keys, func(i, j int) bool {
			a, b := priority(keys[i]), priority(keys[j])
			if a != b {
				return a < b
			}
			return keys[i] < keys[j]
		})
		for _, key := range keys {
			if key == "_truncated" {
				continue
			}
			child := source[key]
			target[key] = child
			candidate, _ := json.Marshal(result)
			if len(candidate) <= MaxRawUsageBytes {
				continue
			}
			delete(target, key)
			if nested, ok := child.(map[string]any); ok {
				kept := map[string]any{}
				target[key] = kept
				collect(nested, kept)
				if len(kept) == 0 {
					delete(target, key)
				}
			}
		}
	}
	if source, ok := value.(map[string]any); ok {
		collect(source, result)
	}
	encoded, _ = json.Marshal(result)
	return string(encoded)
}
