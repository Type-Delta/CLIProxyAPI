package api

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

func TestAnalyticsModuleConfigPropagatesStorageTimeZone(t *testing.T) {
	cfg := &config.Config{Analytics: config.DefaultAnalyticsConfig()}
	cfg.Analytics.StorageTimeZone = "Asia/Bangkok"
	if got := analyticsModuleConfig(cfg).StorageTimeZone; got != "Asia/Bangkok" {
		t.Fatalf("storage time zone=%q, want Asia/Bangkok", got)
	}
}

func TestAnalyticsModuleConfigBuildsCatalogBindings(t *testing.T) {
	cfg := &config.Config{Analytics: config.DefaultAnalyticsConfig(), OpenAICompatibility: []config.OpenAICompatibility{
		{Name: " ZAI ", PricingCatalog: " ZAI-Coding-Plan "},
		{Name: "disabled", PricingCatalog: "openai", Disabled: true},
		{Name: "ZAI", PricingCatalog: "other"},
		{Name: ""},
	}}
	bindings := analyticsModuleConfig(cfg).CatalogBindings
	if len(bindings) != 1 || bindings[0].Provider != util.OpenAICompatibleProviderKey("zai") || bindings[0].Catalog != "zai-coding-plan" {
		t.Fatalf("catalog bindings=%+v", bindings)
	}
}
