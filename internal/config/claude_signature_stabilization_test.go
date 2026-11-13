package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestClaudeSignatureStabilizationCanonicalConfig(t *testing.T) {
	for _, raw := range []string{
		"claude-header-defaults: {signature-stabilization: true}\n",
		"oauth: {providers: {claude: {header-defaults: {signature-stabilization: true}}}}\n",
		"upstream: {claude: {header-defaults: {signature-stabilization: true}}}\n",
	} {
		cfg, err := ParseConfigBytes([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile == nil || !*cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile {
			t.Errorf("canonical signature setting ignored: %s", raw)
		}
	}
	enabled := true
	hd := ClaudeHeaderDefaults{StabilizeDeviceProfile: &enabled}
	raw, err := json.Marshal(hd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"signature-stabilization":true`) || strings.Contains(string(raw), "stabilize-device-profile") {
		t.Errorf("JSON export lacks canonical key: %s", raw)
	}
	raw, err = yaml.Marshal(hd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "signature-stabilization: true") || strings.Contains(string(raw), "stabilize-device-profile") {
		t.Errorf("YAML export lacks canonical key: %s", raw)
	}
}

func TestClaudeSignatureStabilizationAliasesAndPrecedence(t *testing.T) {
	prefixes := []string{"claude-header-defaults", "oauth.providers.claude.header-defaults", "upstream.claude.header-defaults"}
	for _, legacyPath := range prefixes {
		t.Run(legacyPath, func(t *testing.T) {
			root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setYAMLPath(root, legacyPath+".stabilize-device-profile", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
			raw, err := yaml.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := ParseConfigBytes(raw)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile == nil || !*cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile {
				t.Fatal("legacy setting lost")
			}
			for _, canonicalPath := range prefixes {
				copy := deepCopyNode(root)
				setYAMLPath(copy, canonicalPath+".signature-stabilization", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"})
				raw, err = yaml.Marshal(copy)
				if err != nil {
					t.Fatal(err)
				}
				cfg, err = ParseConfigBytes(raw)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile == nil || *cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile {
					t.Fatalf("canonical false at %s lost to %s", canonicalPath, legacyPath)
				}
			}
		})
	}
	for _, raw := range []string{`{"stabilize-device-profile":true}`, `{"stabilize-device-profile":true,"signature-stabilization":false}`, `{"signature-stabilization":null,"stabilize-device-profile":true}`} {
		var hd ClaudeHeaderDefaults
		if err := json.Unmarshal([]byte(raw), &hd); err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.Contains(raw, "null"):
			if hd.StabilizeDeviceProfile != nil {
				t.Fatal("canonical null lost")
			}
		case strings.Contains(raw, "false"):
			if hd.StabilizeDeviceProfile == nil || *hd.StabilizeDeviceProfile {
				t.Fatal("canonical JSON false lost")
			}
		default:
			if hd.StabilizeDeviceProfile == nil || !*hd.StabilizeDeviceProfile {
				t.Fatal("legacy JSON setting lost")
			}
		}
	}
}

func TestClaudeSignatureStabilizationCanonicalSavePreservesUnknownFields(t *testing.T) {
	raw := []byte("upstream: {claude: {header-defaults: {stabilize-device-profile: true, future-setting: retain-me}}}\n")
	normalized, _, err := NormalizeConfigLayout(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(normalized), "stabilize-device-profile") || !strings.Contains(string(normalized), "signature-stabilization: true") || !strings.Contains(string(normalized), "future-setting: retain-me") {
		t.Fatalf("alias migration lost fields: %s", normalized)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if err := NormalizeV8ConfigAliases(doc.Content[0]); err != nil {
		t.Fatal(err)
	}
	if yamlPath(doc.Content[0], "upstream.claude.header-defaults.signature-stabilization") == nil || yamlPath(doc.Content[0], "upstream.claude.header-defaults.future-setting") == nil {
		t.Fatal("API alias normalization lost canonical or unknown fields")
	}
}

func TestClaudeSignatureStabilizationSaveCanonicalAcrossLayouts(t *testing.T) {
	for _, prefix := range []string{"claude-header-defaults", "oauth.providers.claude.header-defaults", "upstream.claude.header-defaults"} {
		t.Run(prefix, func(t *testing.T) {
			root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setYAMLPath(root, prefix+".stabilize-device-profile", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
			setYAMLPath(root, prefix+".future-setting", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "retain-me"})
			raw, err := yaml.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := ParseConfigBytes(raw)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := SaveConfigPreserveComments(path, cfg); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(saved), "stabilize-device-profile") || !strings.Contains(string(saved), "signature-stabilization: true") || !strings.Contains(string(saved), "future-setting: retain-me") {
				t.Fatalf("save reintroduced alias or lost unknown field: %s", saved)
			}
		})
	}
}
