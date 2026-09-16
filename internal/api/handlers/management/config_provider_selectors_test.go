package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type providerSelectorManagementCase struct {
	name      string
	section   string
	body      string
	mutate    func(*Handler, *gin.Context)
	selectors func(*Handler) (string, string)
}

func providerSelectorManagementCases(t *testing.T, probe string) []providerSelectorManagementCase {
	t.Helper()

	putBody := func(fields string) string {
		return fmt.Sprintf(`[{%s,"pricing-catalog":"  PLAN  ","usage-probe":"  %s  "}]`, fields, probe)
	}
	patchBody := fmt.Sprintf(`{"index":0,"value":{"pricing-catalog":"  PLAN  ","usage-probe":"  %s  "}}`, probe)

	return []providerSelectorManagementCase{
		{
			name:    "gemini",
			section: "gemini-api-key",
			body:    putBody(`"api-key":"key"`),
			mutate:  func(h *Handler, c *gin.Context) { h.PutGeminiKeys(c) },
			selectors: func(h *Handler) (string, string) {
				return h.cfg.GeminiKey[0].PricingCatalog, h.cfg.GeminiKey[0].UsageProbe
			},
		},
		{
			name:    "interactions",
			section: "interactions-api-key",
			body:    putBody(`"api-key":"key"`),
			mutate:  func(h *Handler, c *gin.Context) { h.PutInteractionsKeys(c) },
			selectors: func(h *Handler) (string, string) {
				return h.cfg.InteractionsKey[0].PricingCatalog, h.cfg.InteractionsKey[0].UsageProbe
			},
		},
		{
			name:    "claude",
			section: "claude-api-key",
			body:    putBody(`"api-key":"key"`),
			mutate:  func(h *Handler, c *gin.Context) { h.PutClaudeKeys(c) },
			selectors: func(h *Handler) (string, string) {
				return h.cfg.ClaudeKey[0].PricingCatalog, h.cfg.ClaudeKey[0].UsageProbe
			},
		},
		{
			name:    "codex",
			section: "codex-api-key",
			body:    putBody(`"api-key":"key","base-url":"https://codex.example.com"`),
			mutate:  func(h *Handler, c *gin.Context) { h.PutCodexKeys(c) },
			selectors: func(h *Handler) (string, string) {
				return h.cfg.CodexKey[0].PricingCatalog, h.cfg.CodexKey[0].UsageProbe
			},
		},
		{
			name:    "xai",
			section: "xai-api-key",
			body:    putBody(`"api-key":"key","base-url":"https://xai.example.com"`),
			mutate:  func(h *Handler, c *gin.Context) { h.PutXAIKeys(c) },
			selectors: func(h *Handler) (string, string) {
				return h.cfg.XAIKey[0].PricingCatalog, h.cfg.XAIKey[0].UsageProbe
			},
		},
		{
			name:    "vertex",
			section: "vertex-api-key",
			body:    putBody(`"api-key":"key"`),
			mutate:  func(h *Handler, c *gin.Context) { h.PutVertexCompatKeys(c) },
			selectors: func(h *Handler) (string, string) {
				return h.cfg.VertexCompatAPIKey[0].PricingCatalog, h.cfg.VertexCompatAPIKey[0].UsageProbe
			},
		},
		{
			name:    "openai compatibility",
			section: "openai-compatibility",
			body:    fmt.Sprintf(`[{"name":"provider","base-url":"https://openai.example.com","pricing-catalog":"  PLAN  ","usage-probe":"  %s  "}]`, probe),
			mutate:  func(h *Handler, c *gin.Context) { h.PutOpenAICompat(c) },
			selectors: func(h *Handler) (string, string) {
				return h.cfg.OpenAICompatibility[0].PricingCatalog, h.cfg.OpenAICompatibility[0].UsageProbe
			},
		},
		{
			name:    "gemini patch",
			section: "gemini-api-key",
			body:    patchBody,
			mutate:  func(h *Handler, c *gin.Context) { h.PatchGeminiKey(c) },
			selectors: func(h *Handler) (string, string) {
				return h.cfg.GeminiKey[0].PricingCatalog, h.cfg.GeminiKey[0].UsageProbe
			},
		},
		{
			name:    "interactions patch",
			section: "interactions-api-key",
			body:    patchBody,
			mutate:  func(h *Handler, c *gin.Context) { h.PatchInteractionsKey(c) },
			selectors: func(h *Handler) (string, string) {
				return h.cfg.InteractionsKey[0].PricingCatalog, h.cfg.InteractionsKey[0].UsageProbe
			},
		},
		{
			name:    "claude patch",
			section: "claude-api-key",
			body:    patchBody,
			mutate:  func(h *Handler, c *gin.Context) { h.PatchClaudeKey(c) },
			selectors: func(h *Handler) (string, string) {
				return h.cfg.ClaudeKey[0].PricingCatalog, h.cfg.ClaudeKey[0].UsageProbe
			},
		},
		{
			name:    "codex patch",
			section: "codex-api-key",
			body:    patchBody,
			mutate:  func(h *Handler, c *gin.Context) { h.PatchCodexKey(c) },
			selectors: func(h *Handler) (string, string) {
				return h.cfg.CodexKey[0].PricingCatalog, h.cfg.CodexKey[0].UsageProbe
			},
		},
		{
			name:    "xai patch",
			section: "xai-api-key",
			body:    patchBody,
			mutate:  func(h *Handler, c *gin.Context) { h.PatchXAIKey(c) },
			selectors: func(h *Handler) (string, string) {
				return h.cfg.XAIKey[0].PricingCatalog, h.cfg.XAIKey[0].UsageProbe
			},
		},
		{
			name:    "vertex patch",
			section: "vertex-api-key",
			body:    patchBody,
			mutate:  func(h *Handler, c *gin.Context) { h.PatchVertexCompatKey(c) },
			selectors: func(h *Handler) (string, string) {
				return h.cfg.VertexCompatAPIKey[0].PricingCatalog, h.cfg.VertexCompatAPIKey[0].UsageProbe
			},
		},
	}
}

func newProviderSelectorManagementHandler(t *testing.T, cfg *config.Config) *Handler {
	t.Helper()
	return &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t)}
}

func TestProviderSelectorManagementNormalizesSelectors(t *testing.T) {
	for _, test := range providerSelectorManagementCases(t, "OPENCODE-GO") {
		t.Run(test.name, func(t *testing.T) {
			h := newProviderSelectorManagementHandler(t, providerSelectorManagementConfig(test.section))
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(test.body))
			ctx.Request.Header.Set("Content-Type", "application/json")

			test.mutate(h, ctx)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
			}
			catalog, probe := test.selectors(h)
			if catalog != "plan" || probe != "opencode-go" {
				t.Fatalf("selectors = %q/%q, want plan/opencode-go", catalog, probe)
			}
		})
	}
}

func TestProviderSelectorManagementRejectsUnknownProbe(t *testing.T) {
	for _, test := range providerSelectorManagementCases(t, "unsupported") {
		t.Run(test.name, func(t *testing.T) {
			h := newProviderSelectorManagementHandler(t, providerSelectorManagementConfig(test.section))
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(test.body))
			ctx.Request.Header.Set("Content-Type", "application/json")

			test.mutate(h, ctx)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), test.section+"[0].usage-probe") {
				t.Fatalf("body = %s, want section/index validation error", rec.Body.String())
			}
		})
	}
}

func TestProviderSelectorAuthIndexResponsesIncludeSelectors(t *testing.T) {
	tests := []struct {
		section string
		cfg     *config.Config
		get     func(*Handler, *gin.Context)
	}{
		{
			section: "gemini-api-key",
			cfg:     &config.Config{GeminiKey: []config.GeminiKey{{APIKey: "key", PricingCatalog: "plan", UsageProbe: "opencode-go"}}},
			get:     func(h *Handler, c *gin.Context) { h.GetGeminiKeys(c) },
		},
		{
			section: "interactions-api-key",
			cfg:     &config.Config{InteractionsKey: []config.GeminiKey{{APIKey: "key", PricingCatalog: "plan", UsageProbe: "opencode-go"}}},
			get:     func(h *Handler, c *gin.Context) { h.GetInteractionsKeys(c) },
		},
		{
			section: "claude-api-key",
			cfg:     &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "key", PricingCatalog: "plan", UsageProbe: "opencode-go"}}},
			get:     func(h *Handler, c *gin.Context) { h.GetClaudeKeys(c) },
		},
		{
			section: "codex-api-key",
			cfg:     &config.Config{CodexKey: []config.CodexKey{{APIKey: "key", BaseURL: "https://codex.example.com", PricingCatalog: "plan", UsageProbe: "opencode-go"}}},
			get:     func(h *Handler, c *gin.Context) { h.GetCodexKeys(c) },
		},
		{
			section: "xai-api-key",
			cfg:     &config.Config{XAIKey: []config.XAIKey{{APIKey: "key", BaseURL: "https://xai.example.com", PricingCatalog: "plan", UsageProbe: "opencode-go"}}},
			get:     func(h *Handler, c *gin.Context) { h.GetXAIKeys(c) },
		},
		{
			section: "vertex-api-key",
			cfg:     &config.Config{VertexCompatAPIKey: []config.VertexCompatKey{{APIKey: "key", PricingCatalog: "plan", UsageProbe: "opencode-go"}}},
			get:     func(h *Handler, c *gin.Context) { h.GetVertexCompatKeys(c) },
		},
	}
	for _, test := range tests {
		t.Run(test.section, func(t *testing.T) {
			h := NewHandlerWithoutConfigFilePath(test.cfg, nil)
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)

			test.get(h, ctx)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
			}
			var body map[string][]map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			entries := body[test.section]
			if len(entries) != 1 {
				t.Fatalf("entries = %#v, want one entry", entries)
			}
			if entries[0]["pricing-catalog"] != "plan" || entries[0]["usage-probe"] != "opencode-go" {
				t.Fatalf("selectors = %#v, want plan/opencode-go", entries[0])
			}
		})
	}
}

func providerSelectorManagementConfig(section string) *config.Config {
	switch section {
	case "gemini-api-key":
		return &config.Config{GeminiKey: []config.GeminiKey{{APIKey: "key"}}}
	case "interactions-api-key":
		return &config.Config{InteractionsKey: []config.GeminiKey{{APIKey: "key"}}}
	case "claude-api-key":
		return &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "key"}}}
	case "codex-api-key":
		return &config.Config{CodexKey: []config.CodexKey{{APIKey: "key", BaseURL: "https://codex.example.com"}}}
	case "xai-api-key":
		return &config.Config{XAIKey: []config.XAIKey{{APIKey: "key", BaseURL: "https://xai.example.com"}}}
	case "vertex-api-key":
		return &config.Config{VertexCompatAPIKey: []config.VertexCompatKey{{APIKey: "key"}}}
	case "openai-compatibility":
		return &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "provider", BaseURL: "https://openai.example.com"}}}
	default:
		return &config.Config{}
	}
}
