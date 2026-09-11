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

func TestOpenAICompatibilityFieldsNormalizeAndValidate(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`openai-compatibility:
  - name: provider
    base-url: https://example.com
    pricing-catalog: "  ZAI-CODING-PLAN  "
    usage-probe: "  ZAI  "
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	entry := cfg.OpenAICompatibility[0]
	if entry.PricingCatalog != "zai-coding-plan" || entry.UsageProbe != "zai" {
		t.Fatalf("normalized selectors = %q/%q, want zai-coding-plan/zai", entry.PricingCatalog, entry.UsageProbe)
	}

	_, errParse = ParseConfigBytes([]byte(`openai-compatibility:
  - name: provider
    base-url: https://example.com
    usage-probe: unsupported
`))
	if errParse == nil || !strings.Contains(errParse.Error(), "usage-probe") {
		t.Fatalf("ParseConfigBytes() error = %v, want usage-probe validation error", errParse)
	}
}
