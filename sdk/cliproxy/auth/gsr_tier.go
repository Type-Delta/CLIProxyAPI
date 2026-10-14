package auth

import (
	"fmt"
	"strconv"
	"strings"
)

// GSRAllowanceTier is a provider-neutral ordinal used only by greedy-soonest-recovery.
// It deliberately does not represent a provider's raw quota total.
type GSRAllowanceTier uint8

const (
	GSRAllowanceUnknown GSRAllowanceTier = iota
	GSRAllowanceStandard
	GSRAllowancePro
	GSRAllowancePremium
	GSRAllowanceMax
	GSRAllowanceElite
)

// GSRAllowanceTierForAuth returns the configured or provider-reported allowance
// class. Unknown and free credentials retain the neutral base multiplier.
func GSRAllowanceTierForAuth(auth *Auth, provider string) GSRAllowanceTier {
	if auth == nil {
		return GSRAllowanceUnknown
	}
	if resolvedProvider := QuotaRecoveryProviderForAuth(auth); resolvedProvider != "" || strings.TrimSpace(auth.Attributes["usage_probe"]) != "" {
		provider = resolvedProvider
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if strings.Contains(provider, "zai") {
		provider = "zai"
	}
	if strings.Contains(provider, "opencode") {
		provider = "opencode-go"
	}
	for _, raw := range []string{
		auth.Attributes["gsr_tier"],
		auth.Attributes["gsr-tier"],
		stringMetadata(auth.Metadata, "gsr_tier"),
		stringMetadata(auth.Metadata, "gsr-tier"),
	} {
		if tier, ok := parseGSRAllowanceTier(provider, raw); ok {
			return tier
		}
	}
	for _, raw := range []string{auth.Attributes["plan_type"], stringMetadata(auth.Metadata, "plan_type"), tierSignal(auth.Quota.Signals)} {
		if tier, ok := parseGSRProviderPlan(provider, raw); ok {
			return tier
		}
	}
	return GSRAllowanceUnknown
}

func parseGSRProviderPlan(provider, raw string) (GSRAllowanceTier, bool) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	if normalized == "" {
		return GSRAllowanceUnknown, false
	}
	if normalized == "unknown" || normalized == "free" {
		return GSRAllowanceUnknown, true
	}
	if strings.Contains(normalized, "enterprise") {
		return GSRAllowanceElite, true
	}
	switch provider {
	case "codex":
		switch normalized {
		case "pro", "pro-200", "pro200":
			return GSRAllowanceElite, true
		case "prolite", "pro-lite", "pro_lite", "self_serve_business_prolite":
			return GSRAllowancePremium, true
		case "team", "business", "self_serve_business_usage_based":
			return GSRAllowancePro, true
		case "plus":
			return GSRAllowanceStandard, true
		}
	case "claude":
		switch {
		case strings.Contains(normalized, "max"):
			return GSRAllowanceMax, true
		case strings.Contains(normalized, "team") || strings.Contains(normalized, "business"):
			return GSRAllowancePremium, true
		case strings.Contains(normalized, "pro"):
			return GSRAllowancePro, true
		}
	case "zai":
		switch normalized {
		case "lite", "standard":
			return GSRAllowanceStandard, true
		case "pro", "premium":
			return GSRAllowancePremium, true
		case "max", "ultra":
			return GSRAllowanceMax, true
		}
	case "kiro":
		normalized = strings.TrimSpace(strings.TrimPrefix(normalized, "kiro"))
		switch normalized {
		case "free", "unknown":
			return GSRAllowanceUnknown, true
		case "pro":
			return GSRAllowancePro, true
		case "pro+", "pro plus", "proplus":
			return GSRAllowancePremium, true
		case "power":
			return GSRAllowanceMax, true
		}
	default:
		if strings.Contains(normalized, "max") {
			return GSRAllowanceMax, true
		}
		if strings.Contains(normalized, "premium") || strings.Contains(normalized, "pro") {
			return GSRAllowancePro, true
		}
		if normalized == "standard" || normalized == "plus" || normalized == "lite" || normalized == "go" {
			return GSRAllowanceStandard, true
		}
	}
	return GSRAllowanceUnknown, false
}

func stringMetadata(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok {
		return ""
	}
	return fmt.Sprint(value)
}

func tierSignal(signals map[string]string) string {
	for key, value := range signals {
		lower := strings.ToLower(strings.TrimSpace(key))
		if lower == "x-codex-plan-type" || lower == "plan_type" ||
			strings.HasSuffix(lower, "-plan-type") || strings.HasSuffix(lower, "_plan_type") ||
			strings.HasSuffix(lower, "-gsr-tier") || strings.HasSuffix(lower, "_gsr_tier") {
			return value
		}
	}
	return ""
}

func parseGSRAllowanceTier(provider, raw string) (GSRAllowanceTier, bool) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	if normalized == "" {
		return GSRAllowanceUnknown, false
	}
	if numeric, errParse := strconv.Atoi(normalized); errParse == nil && numeric >= 0 && numeric <= int(GSRAllowanceElite) {
		return GSRAllowanceTier(numeric), true
	}
	if normalized == "unknown" || normalized == "free" {
		return GSRAllowanceUnknown, true
	}
	if normalized == "standard" {
		return GSRAllowanceStandard, true
	}
	if normalized == "pro" {
		return GSRAllowancePro, true
	}
	if normalized == "premium" {
		return GSRAllowancePremium, true
	}
	if normalized == "max" {
		return GSRAllowanceMax, true
	}
	if normalized == "elite" {
		return GSRAllowanceElite, true
	}
	return GSRAllowanceUnknown, false
}

func gsrAllowanceMultiplier(tier GSRAllowanceTier) int64 {
	switch tier {
	case GSRAllowanceStandard:
		return 2
	case GSRAllowancePro:
		return 4
	case GSRAllowancePremium:
		return 6
	case GSRAllowanceMax:
		return 8
	case GSRAllowanceElite:
		return 12
	default:
		return 1
	}
}

func multiplyGSRWeight(weight, multiplier int64) int64 {
	if weight <= 0 || multiplier <= 0 {
		return 0
	}
	if weight > int64(^uint64(0)>>1)/multiplier {
		return int64(^uint64(0) >> 1)
	}
	return weight * multiplier
}
