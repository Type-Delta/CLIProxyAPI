package config

import (
	"fmt"
	"strings"
)

// ValidateCredentialLabels rejects duplicate non-empty labels within each
// credential list. Labels are compared exactly; empty labels are ignored.
func (cfg *Config) ValidateCredentialLabels() error {
	if cfg == nil {
		return nil
	}
	if err := validateCredentialLabelList("claude-api-key", len(cfg.ClaudeKey), func(index int) string {
		return cfg.ClaudeKey[index].Label
	}); err != nil {
		return err
	}
	if err := validateCredentialLabelList("codex-api-key", len(cfg.CodexKey), func(index int) string {
		return cfg.CodexKey[index].Label
	}); err != nil {
		return err
	}
	if err := validateCredentialLabelList("xai-api-key", len(cfg.XAIKey), func(index int) string {
		return cfg.XAIKey[index].Label
	}); err != nil {
		return err
	}
	if err := validateCredentialLabelList("gemini-api-key", len(cfg.GeminiKey), func(index int) string {
		return cfg.GeminiKey[index].Label
	}); err != nil {
		return err
	}
	if err := validateCredentialLabelList("interactions-api-key", len(cfg.InteractionsKey), func(index int) string {
		return cfg.InteractionsKey[index].Label
	}); err != nil {
		return err
	}
	if err := validateCredentialLabelList("vertex-api-key", len(cfg.VertexCompatAPIKey), func(index int) string {
		return cfg.VertexCompatAPIKey[index].Label
	}); err != nil {
		return err
	}
	for providerIndex := range cfg.OpenAICompatibility {
		entries := cfg.OpenAICompatibility[providerIndex].APIKeyEntries
		field := fmt.Sprintf("openai-compatibility[%d].api-key-entries", providerIndex)
		if err := validateCredentialLabelList(field, len(entries), func(index int) string {
			return entries[index].Label
		}); err != nil {
			return err
		}
	}
	return nil
}

func validateCredentialLabelList(field string, count int, labelAt func(int) string) error {
	seen := make(map[string]int, count)
	for index := 0; index < count; index++ {
		label := labelAt(index)
		if label == "" {
			continue
		}
		if previous, exists := seen[label]; exists {
			return fmt.Errorf("duplicate credential label %q in %s at entries %d and %d", label, field, previous, index)
		}
		seen[label] = index
	}
	return nil
}

// NormalizeOpenAICompatibilityFields canonicalizes the catalog and usage
// probe selectors without changing provider identity or credential IDs.
func (cfg *Config) NormalizeOpenAICompatibilityFields() {
	if cfg == nil {
		return
	}
	for index := range cfg.OpenAICompatibility {
		cfg.OpenAICompatibility[index].PricingCatalog = strings.ToLower(strings.TrimSpace(cfg.OpenAICompatibility[index].PricingCatalog))
		cfg.OpenAICompatibility[index].UsageProbe = strings.ToLower(strings.TrimSpace(cfg.OpenAICompatibility[index].UsageProbe))
	}
}

// ValidateOpenAICompatibilityFields validates the supported usage probe names.
func (cfg *Config) ValidateOpenAICompatibilityFields() error {
	if cfg == nil {
		return nil
	}
	for index, entry := range cfg.OpenAICompatibility {
		probe := strings.ToLower(strings.TrimSpace(entry.UsageProbe))
		if probe != "" && probe != "zai" {
			return fmt.Errorf("openai-compatibility[%d].usage-probe: unsupported value %q; must be empty or zai", index, entry.UsageProbe)
		}
	}
	return nil
}
