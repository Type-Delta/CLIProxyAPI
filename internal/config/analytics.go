package config

import (
	"fmt"
	"net/netip"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var analyticsFields = map[string]struct{}{
	"enabled":                   {},
	"path":                      {},
	"queue-capacity":            {},
	"batch-size":                {},
	"flush-interval":            {},
	"hot-retention-days":        {},
	"circuit-failure-threshold": {},
	"max-storage-bytes":         {},
	"min-free-bytes":            {},
	"storage-time-zone":         {},
	"privacy":                   {},
	"viewer":                    {},
}

// AnalyticsConfig keeps analytics decoding failures local to CPAUK. YAML
// syntax errors still fail the entire document before this decoder runs.
type AnalyticsConfig struct {
	Enabled                 bool                   `yaml:"enabled" json:"enabled"`
	Path                    string                 `yaml:"path" json:"path"`
	QueueCapacity           int                    `yaml:"queue-capacity" json:"queue_capacity"`
	BatchSize               int                    `yaml:"batch-size" json:"batch_size"`
	FlushInterval           time.Duration          `yaml:"flush-interval" json:"flush_interval"`
	HotRetentionDays        int                    `yaml:"hot-retention-days" json:"hot_retention_days"`
	CircuitFailureThreshold int                    `yaml:"circuit-failure-threshold" json:"circuit_failure_threshold"`
	MaxStorageBytes         int64                  `yaml:"max-storage-bytes" json:"max_storage_bytes"`
	MinFreeBytes            int64                  `yaml:"min-free-bytes" json:"min_free_bytes"`
	StorageTimeZone         string                 `yaml:"storage-time-zone" json:"storage_time_zone"`
	Privacy                 AnalyticsPrivacyConfig `yaml:"privacy" json:"privacy"`
	Viewer                  AnalyticsViewerConfig  `yaml:"viewer" json:"viewer"`

	present bool
	problem *AnalyticsConfigProblem
}

// AnalyticsPrivacyConfig contains analytics-local privacy switches.
type AnalyticsPrivacyConfig struct {
	StoreCredentialID bool `yaml:"store-credential-id" json:"store_credential_id"`
}

// AnalyticsViewerConfig controls the separate scoped-viewer trust boundary.
type AnalyticsViewerConfig struct {
	TrustedProxyCIDRs []string `yaml:"trusted-proxy-cidrs" json:"trusted_proxy_cidrs"`
	AllowLoopbackHTTP bool     `yaml:"allow-loopback-http" json:"allow_loopback_http"`
}

// AnalyticsConfigProblem is safe to expose through analytics health.
type AnalyticsConfigProblem struct {
	Category string `json:"category"`
	Field    string `json:"field,omitempty"`
}

// DefaultAnalyticsConfig returns the opt-in disabled configuration.
func DefaultAnalyticsConfig() AnalyticsConfig {
	return AnalyticsConfig{
		QueueCapacity:           8192,
		BatchSize:               256,
		FlushInterval:           250 * time.Millisecond,
		HotRetentionDays:        90,
		CircuitFailureThreshold: 5,
		MaxStorageBytes:         5 * 1024 * 1024 * 1024,
		MinFreeBytes:            512 * 1024 * 1024,
		StorageTimeZone:         "UTC",
		Privacy:                 AnalyticsPrivacyConfig{StoreCredentialID: true},
	}
}

// UnmarshalYAML deliberately consumes analytics-local failures without
// returning them to the top-level configuration decoder.
func (c *AnalyticsConfig) UnmarshalYAML(node *yaml.Node) error {
	if c == nil {
		return nil
	}
	*c = DefaultAnalyticsConfig()
	c.present = true
	c.problem = nil
	if node == nil || node.Kind != yaml.MappingNode {
		c.problem = &AnalyticsConfigProblem{Category: "invalid_kind", Field: "analytics"}
		return nil
	}
	seen := make(map[string]struct{}, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		field := node.Content[i].Value
		if _, duplicate := seen[field]; duplicate {
			c.problem = &AnalyticsConfigProblem{Category: "duplicate_field", Field: field}
			return nil
		}
		seen[field] = struct{}{}
		if _, allowed := analyticsFields[field]; !allowed {
			c.problem = &AnalyticsConfigProblem{Category: "unknown_field", Field: field}
			return nil
		}
		if field == "privacy" {
			if problem := validateAnalyticsPrivacyNode(node.Content[i+1]); problem != nil {
				c.problem = problem
				return nil
			}
		}
		if field == "viewer" {
			if problem := validateAnalyticsViewerNode(node.Content[i+1]); problem != nil {
				c.problem = problem
				return nil
			}
		}
	}

	type plainAnalyticsConfig AnalyticsConfig
	decoded := plainAnalyticsConfig(DefaultAnalyticsConfig())
	if errDecode := node.Decode(&decoded); errDecode != nil {
		c.problem = &AnalyticsConfigProblem{Category: "invalid_type", Field: "analytics"}
		return nil
	}
	decoded.present = true
	*c = AnalyticsConfig(decoded)
	if field := c.invalidField(); field != "" {
		c.problem = &AnalyticsConfigProblem{Category: "invalid_value", Field: field}
	}
	return nil
}

func (c AnalyticsConfig) invalidField() string {
	if strings.TrimSpace(c.Path) != c.Path {
		return "path"
	}
	if c.QueueCapacity < 1 || int64(c.QueueCapacity)*4096 > 32*1024*1024 {
		return "queue-capacity"
	}
	if c.BatchSize < 1 || c.BatchSize > c.QueueCapacity {
		return "batch-size"
	}
	if c.FlushInterval < time.Millisecond || c.FlushInterval > time.Minute {
		return "flush-interval"
	}
	if c.HotRetentionDays < 1 {
		return "hot-retention-days"
	}
	if c.CircuitFailureThreshold < 1 {
		return "circuit-failure-threshold"
	}
	if c.MaxStorageBytes < 0 || c.MinFreeBytes < 0 || (c.MaxStorageBytes == 0 && c.MinFreeBytes == 0) {
		return "storage-budget"
	}
	if c.StorageTimeZone == "" || strings.TrimSpace(c.StorageTimeZone) != c.StorageTimeZone {
		return "storage-time-zone"
	}
	if _, err := time.LoadLocation(c.StorageTimeZone); err != nil {
		return "storage-time-zone"
	}
	if len(c.Viewer.TrustedProxyCIDRs) > 64 {
		return "viewer.trusted-proxy-cidrs"
	}
	seenProxyCIDRs := make(map[netip.Prefix]struct{}, len(c.Viewer.TrustedProxyCIDRs))
	for _, rawPrefix := range c.Viewer.TrustedProxyCIDRs {
		if rawPrefix == "" || strings.TrimSpace(rawPrefix) != rawPrefix {
			return "viewer.trusted-proxy-cidrs"
		}
		prefix, errParse := netip.ParsePrefix(rawPrefix)
		if errParse != nil || prefix != prefix.Masked() {
			return "viewer.trusted-proxy-cidrs"
		}
		if _, duplicate := seenProxyCIDRs[prefix]; duplicate {
			return "viewer.trusted-proxy-cidrs"
		}
		seenProxyCIDRs[prefix] = struct{}{}
	}
	return ""
}

func validateAnalyticsPrivacyNode(node *yaml.Node) *AnalyticsConfigProblem {
	if node == nil || node.Kind != yaml.MappingNode {
		return &AnalyticsConfigProblem{Category: "invalid_kind", Field: "privacy"}
	}
	seen := false
	for i := 0; i+1 < len(node.Content); i += 2 {
		field := node.Content[i].Value
		if field != "store-credential-id" {
			return &AnalyticsConfigProblem{Category: "unknown_field", Field: fmt.Sprintf("privacy.%s", field)}
		}
		if seen {
			return &AnalyticsConfigProblem{Category: "duplicate_field", Field: "privacy.store-credential-id"}
		}
		seen = true
	}
	return nil
}

func validateAnalyticsViewerNode(node *yaml.Node) *AnalyticsConfigProblem {
	if node == nil || node.Kind != yaml.MappingNode {
		return &AnalyticsConfigProblem{Category: "invalid_kind", Field: "viewer"}
	}
	seen := make(map[string]struct{}, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		field := node.Content[i].Value
		if _, duplicate := seen[field]; duplicate {
			return &AnalyticsConfigProblem{Category: "duplicate_field", Field: "viewer." + field}
		}
		seen[field] = struct{}{}
		switch field {
		case "trusted-proxy-cidrs", "allow-loopback-http":
		default:
			return &AnalyticsConfigProblem{Category: "unknown_field", Field: "viewer." + field}
		}
	}
	return nil
}

// Problem reports an isolated, redacted analytics decode or validation error.
func (c AnalyticsConfig) Problem() *AnalyticsConfigProblem {
	if c.problem == nil {
		return nil
	}
	copyProblem := *c.problem
	return &copyProblem
}

// Present reports whether the source document contained an analytics node.
func (c AnalyticsConfig) Present() bool { return c.present }
