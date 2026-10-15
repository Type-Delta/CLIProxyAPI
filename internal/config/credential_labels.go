package config

import (
	"fmt"
	"math"
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
		if err := entry.QuotaReserveConfig.validate("openai-compatibility", index); err != nil {
			return err
		}
	}
	for section, entries := range map[string][]QuotaReserveConfig{
		"claude-api-key":       quotaReserveOf(cfg.ClaudeKey, func(k ClaudeKey) QuotaReserveConfig { return k.QuotaReserveConfig }),
		"codex-api-key":        quotaReserveOf(cfg.CodexKey, func(k CodexKey) QuotaReserveConfig { return k.QuotaReserveConfig }),
		"xai-api-key":          quotaReserveOf(cfg.XAIKey, func(k XAIKey) QuotaReserveConfig { return k.QuotaReserveConfig }),
		"gemini-api-key":       quotaReserveOf(cfg.GeminiKey, func(k GeminiKey) QuotaReserveConfig { return k.QuotaReserveConfig }),
		"interactions-api-key": quotaReserveOf(cfg.InteractionsKey, func(k GeminiKey) QuotaReserveConfig { return k.QuotaReserveConfig }),
		"vertex-api-key":       quotaReserveOf(cfg.VertexCompatAPIKey, func(k VertexCompatKey) QuotaReserveConfig { return k.QuotaReserveConfig }),
	} {
		for index, reserves := range entries {
			if err := reserves.validate(section, index); err != nil {
				return err
			}
		}
	}
	return nil
}

func quotaReserveOf[T any](entries []T, get func(T) QuotaReserveConfig) []QuotaReserveConfig {
	out := make([]QuotaReserveConfig, len(entries))
	for index, entry := range entries {
		out[index] = get(entry)
	}
	return out
}

// validate checks reserve percents and bypass key IDs. It mirrors the auth
// package's NormalizeQuotaReserves rules without importing it.
func (c QuotaReserveConfig) validate(section string, index int) error {
	for barID, percent := range c.QuotaReserve {
		if strings.TrimSpace(barID) == "" {
			return fmt.Errorf("%s[%d].quota-reserve: bar id is required", section, index)
		}
		if math.IsNaN(percent) || percent < 0 || percent >= 100 {
			return fmt.Errorf("%s[%d].quota-reserve.%s: must be a number from 0 to 99", section, index, barID)
		}
	}
	for _, keyID := range c.QuotaReserveBypassKeys {
		if !isAPIKeyIDHex(strings.ToLower(strings.TrimSpace(keyID))) {
			return fmt.Errorf("%s[%d].quota-reserve-bypass-keys: entries must be API key ids", section, index)
		}
	}
	return nil
}

func isAPIKeyIDHex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
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
