package api

import (
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
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

func TestAnalyticsModuleConfigBuildsCatalogBindingsForAPIKeyProviders(t *testing.T) {
	cfg := &config.Config{
		Analytics: config.DefaultAnalyticsConfig(),
		GeminiKey: []config.GeminiKey{
			{APIKey: "gemini-key", PricingCatalog: " Google "},
		},
		InteractionsKey: []config.GeminiKey{
			{APIKey: "interactions-key", PricingCatalog: " Google "},
		},
		ClaudeKey: []config.ClaudeKey{
			{APIKey: "claude-key", PricingCatalog: ""},
			{APIKey: "claude-key", PricingCatalog: " Anthropic "},
			{APIKey: "claude-key", PricingCatalog: "ignored"},
			{PricingCatalog: "empty-entry"},
		},
		CodexKey: []config.CodexKey{
			{APIKey: "codex-key", PricingCatalog: " OpenAI "},
		},
		XAIKey: []config.XAIKey{
			{APIKey: "xai-key", PricingCatalog: " XAI "},
		},
		VertexCompatAPIKey: []config.VertexCompatKey{
			{APIKey: "vertex-key", PricingCatalog: " Google-Vertex "},
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{Name: "zai", PricingCatalog: " ZAI-Coding-Plan "},
		},
	}

	want := []cpauk.CatalogBinding{
		{Provider: "claude", Catalog: "anthropic"},
		{Provider: "codex", Catalog: "openai"},
		{Provider: "gemini", Catalog: "google"},
		{Provider: "gemini-interactions", Catalog: "google"},
		{Provider: util.OpenAICompatibleProviderKey("zai"), Catalog: "zai-coding-plan"},
		{Provider: "vertex", Catalog: "google-vertex"},
		{Provider: "xai", Catalog: "xai"},
	}
	if got := analyticsModuleConfig(cfg).CatalogBindings; !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog bindings=%+v, want %+v", got, want)
	}
}

func TestAnalyticsModuleConfigSkipsEmptyCatalogForAPIKeyProviders(t *testing.T) {
	cfg := &config.Config{
		Analytics: config.DefaultAnalyticsConfig(),
		ClaudeKey: []config.ClaudeKey{{APIKey: "claude-key"}},
	}
	if got := analyticsModuleConfig(cfg).CatalogBindings; len(got) != 0 {
		t.Fatalf("catalog bindings=%+v, want none", got)
	}
}

func TestAnalyticsModuleConfigPreservesEmptyOpenAICompatibleCatalogBinding(t *testing.T) {
	cfg := &config.Config{
		Analytics:           config.DefaultAnalyticsConfig(),
		OpenAICompatibility: []config.OpenAICompatibility{{Name: "zai"}},
	}
	got := analyticsModuleConfig(cfg).CatalogBindings
	if len(got) != 1 || got[0].Provider != util.OpenAICompatibleProviderKey("zai") || got[0].Catalog != "" {
		t.Fatalf("catalog bindings=%+v", got)
	}
}
