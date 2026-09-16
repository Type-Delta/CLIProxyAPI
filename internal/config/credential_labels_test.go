package config

import (
	"strings"
	"testing"
)

func TestValidateCredentialLabelsRejectsDuplicatesPerList(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "claude",
			cfg:  Config{ClaudeKey: []ClaudeKey{{Label: "same"}, {Label: "same"}}},
		},
		{
			name: "interactions",
			cfg:  Config{InteractionsKey: []GeminiKey{{Label: "same"}, {Label: "same"}}},
		},
		{
			name: "openai compatibility keys",
			cfg: Config{OpenAICompatibility: []OpenAICompatibility{{APIKeyEntries: []OpenAICompatibilityAPIKey{
				{Label: "same"}, {Label: "same"},
			}}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errValidate := test.cfg.ValidateCredentialLabels()
			if errValidate == nil || !strings.Contains(errValidate.Error(), "duplicate credential label") {
				t.Fatalf("ValidateCredentialLabels() = %v, want duplicate label error", errValidate)
			}
		})
	}
}

func TestValidateCredentialLabelsAllowsEmptyAndDistinctLabels(t *testing.T) {
	cfg := Config{
		GeminiKey: []GeminiKey{{Label: ""}, {Label: "team-a"}, {Label: "team-b"}},
		VertexCompatAPIKey: []VertexCompatKey{
			{Label: ""},
			{Label: "team-a"},
		},
	}
	if errValidate := cfg.ValidateCredentialLabels(); errValidate != nil {
		t.Fatalf("ValidateCredentialLabels() rejected distinct labels in separate lists: %v", errValidate)
	}
}

func TestProviderSelectorFieldsNormalizeAndValidate(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`claude-api-key:
  - api-key: claude-key
    pricing-catalog: "  CLAUDE-PLAN  "
    usage-probe: "  ZAI  "
codex-api-key:
  - api-key: codex-key
    base-url: https://codex.example.com
    pricing-catalog: "  CODEX-PLAN  "
    usage-probe: "  OPENCODE-GO  "
xai-api-key:
  - api-key: xai-key
    base-url: https://xai.example.com
    pricing-catalog: "  XAI-PLAN  "
    usage-probe: "  ZAI  "
gemini-api-key:
  - api-key: gemini-key
    pricing-catalog: "  GEMINI-PLAN  "
    usage-probe: "  OPENCODE-GO  "
interactions-api-key:
  - api-key: interactions-key
    pricing-catalog: "  INTERACTIONS-PLAN  "
    usage-probe: "  ZAI  "
vertex-api-key:
  - api-key: vertex-key
    pricing-catalog: "  VERTEX-PLAN  "
    usage-probe: "  OPENCODE-GO  "
openai-compatibility:
  - name: provider
    base-url: https://example.com
    pricing-catalog: "  ZAI-CODING-PLAN  "
    usage-probe: "  ZAI  "
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	selectors := []struct {
		section string
		catalog string
		probe   string
	}{
		{section: "claude-api-key", catalog: cfg.ClaudeKey[0].PricingCatalog, probe: cfg.ClaudeKey[0].UsageProbe},
		{section: "codex-api-key", catalog: cfg.CodexKey[0].PricingCatalog, probe: cfg.CodexKey[0].UsageProbe},
		{section: "xai-api-key", catalog: cfg.XAIKey[0].PricingCatalog, probe: cfg.XAIKey[0].UsageProbe},
		{section: "gemini-api-key", catalog: cfg.GeminiKey[0].PricingCatalog, probe: cfg.GeminiKey[0].UsageProbe},
		{section: "interactions-api-key", catalog: cfg.InteractionsKey[0].PricingCatalog, probe: cfg.InteractionsKey[0].UsageProbe},
		{section: "vertex-api-key", catalog: cfg.VertexCompatAPIKey[0].PricingCatalog, probe: cfg.VertexCompatAPIKey[0].UsageProbe},
		{section: "openai-compatibility", catalog: cfg.OpenAICompatibility[0].PricingCatalog, probe: cfg.OpenAICompatibility[0].UsageProbe},
	}
	want := map[string][2]string{
		"claude-api-key":       {"claude-plan", "zai"},
		"codex-api-key":        {"codex-plan", "opencode-go"},
		"xai-api-key":          {"xai-plan", "zai"},
		"gemini-api-key":       {"gemini-plan", "opencode-go"},
		"interactions-api-key": {"interactions-plan", "zai"},
		"vertex-api-key":       {"vertex-plan", "opencode-go"},
		"openai-compatibility": {"zai-coding-plan", "zai"},
	}
	for _, selector := range selectors {
		if selector.catalog != want[selector.section][0] || selector.probe != want[selector.section][1] {
			t.Fatalf("%s selectors = %q/%q, want %q/%q", selector.section, selector.catalog, selector.probe, want[selector.section][0], want[selector.section][1])
		}
	}
}

func TestProviderSelectorFieldsRejectUnsupportedUsageProbe(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		section string
	}{
		{name: "claude", cfg: Config{ClaudeKey: []ClaudeKey{{}, {UsageProbe: "unsupported"}}}, section: "claude-api-key[1].usage-probe"},
		{name: "codex", cfg: Config{CodexKey: []CodexKey{{}, {UsageProbe: "unsupported"}}}, section: "codex-api-key[1].usage-probe"},
		{name: "xai", cfg: Config{XAIKey: []XAIKey{{}, {UsageProbe: "unsupported"}}}, section: "xai-api-key[1].usage-probe"},
		{name: "gemini", cfg: Config{GeminiKey: []GeminiKey{{}, {UsageProbe: "unsupported"}}}, section: "gemini-api-key[1].usage-probe"},
		{name: "interactions", cfg: Config{InteractionsKey: []GeminiKey{{}, {UsageProbe: "unsupported"}}}, section: "interactions-api-key[1].usage-probe"},
		{name: "vertex", cfg: Config{VertexCompatAPIKey: []VertexCompatKey{{}, {UsageProbe: "unsupported"}}}, section: "vertex-api-key[1].usage-probe"},
		{name: "openai", cfg: Config{OpenAICompatibility: []OpenAICompatibility{{}, {UsageProbe: "unsupported"}}}, section: "openai-compatibility[1].usage-probe"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errValidate := test.cfg.ValidateProviderSelectorFields()
			if errValidate == nil || !strings.Contains(errValidate.Error(), test.section) || !strings.Contains(errValidate.Error(), "opencode-go") {
				t.Fatalf("ValidateProviderSelectorFields() = %v, want error containing %q and supported probes", errValidate, test.section)
			}
		})
	}
}
