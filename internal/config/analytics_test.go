package config

import (
	"testing"
	"time"
)

func TestAnalyticsConfigDefaultsAndValidDecode(t *testing.T) {
	absent, errAbsent := ParseConfigBytes([]byte("port: 8317\n"))
	if errAbsent != nil {
		t.Fatal(errAbsent)
	}
	if absent.Analytics.Enabled || absent.Analytics.Problem() != nil || absent.Analytics.QueueCapacity != 8192 {
		t.Fatalf("absent analytics = %#v problem=%#v", absent.Analytics, absent.Analytics.Problem())
	}

	valid, errValid := ParseConfigBytes([]byte(`
port: 8317
analytics:
  enabled: true
  queue-capacity: 4096
  batch-size: 128
  flush-interval: 500ms
  storage-time-zone: Asia/Bangkok
  privacy:
    store-credential-id: false
`))
	if errValid != nil {
		t.Fatal(errValid)
	}
	if problem := valid.Analytics.Problem(); problem != nil {
		t.Fatalf("valid analytics problem = %#v", problem)
	}
	if !valid.Analytics.Enabled || valid.Analytics.QueueCapacity != 4096 || valid.Analytics.BatchSize != 128 || valid.Analytics.FlushInterval != 500*time.Millisecond || valid.Analytics.StorageTimeZone != "Asia/Bangkok" {
		t.Fatalf("valid analytics = %#v", valid.Analytics)
	}
}

func TestAnalyticsConfigFailuresAreIsolated(t *testing.T) {
	tests := []struct {
		name     string
		node     string
		category string
		field    string
	}{
		{name: "wrong node kind", node: "[]", category: "invalid_kind", field: "analytics"},
		{name: "invalid type", node: "{enabled: yes, queue-capacity: nope}", category: "invalid_type", field: "analytics"},
		{name: "unknown field", node: "{enabled: true, surprise: true}", category: "unknown_field", field: "surprise"},
		{name: "unknown privacy field", node: "{enabled: true, privacy: {store-auth-id: true}}", category: "unknown_field", field: "privacy.store-auth-id"},
		{name: "semantic error", node: "{enabled: true, batch-size: -1}", category: "invalid_value", field: "batch-size"},
		{name: "invalid storage zone", node: "{enabled: true, storage-time-zone: Not/AZone}", category: "invalid_value", field: "storage-time-zone"},
		{name: "invalid viewer proxy", node: "{viewer: {trusted-proxy-cidrs: [not-a-cidr]}}", category: "invalid_value", field: "viewer.trusted-proxy-cidrs"},
		{name: "non-canonical viewer proxy", node: "{viewer: {trusted-proxy-cidrs: [192.0.2.1/24]}}", category: "invalid_value", field: "viewer.trusted-proxy-cidrs"},
		{name: "duplicate viewer proxy", node: "{viewer: {trusted-proxy-cidrs: [192.0.2.0/24, 192.0.2.0/24]}}", category: "invalid_value", field: "viewer.trusted-proxy-cidrs"},
		{name: "duplicate field", node: "{enabled: true, enabled: false}", category: "duplicate_field", field: "enabled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte("port: 9123\ndebug: true\nanalytics: " + test.node + "\n"))
			if errParse != nil {
				t.Fatalf("ParseConfigBytes() returned top-level error: %v", errParse)
			}
			if cfg.Port != 9123 || !cfg.Debug {
				t.Fatalf("valid CPA fields were lost: %#v", cfg)
			}
			problem := cfg.Analytics.Problem()
			if problem == nil || problem.Category != test.category || problem.Field != test.field {
				t.Fatalf("problem = %#v, want %s/%s", problem, test.category, test.field)
			}
		})
	}
}

func TestAnalyticsDoesNotMaskYAMLSyntaxError(t *testing.T) {
	if _, errParse := ParseConfigBytes([]byte("port: [\nanalytics: true\n")); errParse == nil {
		t.Fatal("ParseConfigBytes() accepted malformed YAML")
	}
}
