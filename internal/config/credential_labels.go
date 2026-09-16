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

// NormalizeProviderSelectorFields canonicalizes catalog and usage probe
// selectors without changing provider identity or credential IDs.
func (cfg *Config) NormalizeProviderSelectorFields() {
	if cfg == nil {
		return
	}
	for index := range cfg.ClaudeKey {
		normalizeProviderSelectorFields(&cfg.ClaudeKey[index].PricingCatalog, &cfg.ClaudeKey[index].UsageProbe)
	}
	for index := range cfg.CodexKey {
		normalizeProviderSelectorFields(&cfg.CodexKey[index].PricingCatalog, &cfg.CodexKey[index].UsageProbe)
	}
	for index := range cfg.XAIKey {
		normalizeProviderSelectorFields(&cfg.XAIKey[index].PricingCatalog, &cfg.XAIKey[index].UsageProbe)
	}
	for index := range cfg.GeminiKey {
		normalizeProviderSelectorFields(&cfg.GeminiKey[index].PricingCatalog, &cfg.GeminiKey[index].UsageProbe)
	}
	for index := range cfg.InteractionsKey {
		normalizeProviderSelectorFields(&cfg.InteractionsKey[index].PricingCatalog, &cfg.InteractionsKey[index].UsageProbe)
	}
	for index := range cfg.VertexCompatAPIKey {
		normalizeProviderSelectorFields(&cfg.VertexCompatAPIKey[index].PricingCatalog, &cfg.VertexCompatAPIKey[index].UsageProbe)
	}
	for index := range cfg.OpenAICompatibility {
		normalizeProviderSelectorFields(&cfg.OpenAICompatibility[index].PricingCatalog, &cfg.OpenAICompatibility[index].UsageProbe)
	}
}

// ValidateProviderSelectorFields validates the supported usage probe names.
func (cfg *Config) ValidateProviderSelectorFields() error {
	if cfg == nil {
		return nil
	}
	if err := validateUsageProbeList("claude-api-key", len(cfg.ClaudeKey), func(index int) string {
		return cfg.ClaudeKey[index].UsageProbe
	}); err != nil {
		return err
	}
	if err := validateUsageProbeList("codex-api-key", len(cfg.CodexKey), func(index int) string {
		return cfg.CodexKey[index].UsageProbe
	}); err != nil {
		return err
	}
	if err := validateUsageProbeList("xai-api-key", len(cfg.XAIKey), func(index int) string {
		return cfg.XAIKey[index].UsageProbe
	}); err != nil {
		return err
	}
	if err := validateUsageProbeList("gemini-api-key", len(cfg.GeminiKey), func(index int) string {
		return cfg.GeminiKey[index].UsageProbe
	}); err != nil {
		return err
	}
	if err := validateUsageProbeList("interactions-api-key", len(cfg.InteractionsKey), func(index int) string {
		return cfg.InteractionsKey[index].UsageProbe
	}); err != nil {
		return err
	}
	if err := validateUsageProbeList("vertex-api-key", len(cfg.VertexCompatAPIKey), func(index int) string {
		return cfg.VertexCompatAPIKey[index].UsageProbe
	}); err != nil {
		return err
	}
	for index, entry := range cfg.OpenAICompatibility {
		if err := validateUsageProbe("openai-compatibility", index, entry.UsageProbe); err != nil {
			return err
		}
	}
	return nil
}

func normalizeProviderSelectorFields(pricingCatalog, usageProbe *string) {
	*pricingCatalog = strings.ToLower(strings.TrimSpace(*pricingCatalog))
	*usageProbe = strings.ToLower(strings.TrimSpace(*usageProbe))
}

func validateUsageProbeList(section string, count int, usageProbeAt func(int) string) error {
	for index := 0; index < count; index++ {
		if err := validateUsageProbe(section, index, usageProbeAt(index)); err != nil {
			return err
		}
	}
	return nil
}

func validateUsageProbe(section string, index int, usageProbe string) error {
	switch strings.ToLower(strings.TrimSpace(usageProbe)) {
	case "", "zai", "opencode-go":
		return nil
	default:
		return fmt.Errorf("%s[%d].usage-probe: unsupported value %q; must be empty, zai or opencode-go", section, index, usageProbe)
	}
}
