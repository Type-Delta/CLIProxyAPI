package synthesizer

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestConfigSynthesizerPropagatesCredentialLabels(t *testing.T) {
	ctx := &SynthesisContext{
		Config: &config.Config{
			GeminiKey:       []config.GeminiKey{{APIKey: "gemini", Label: "Gemini team"}},
			InteractionsKey: []config.GeminiKey{{APIKey: "interactions", Label: "Interactions team"}},
			ClaudeKey:       []config.ClaudeKey{{APIKey: "claude", Label: "Claude team"}},
			CodexKey:        []config.CodexKey{{APIKey: "codex", BaseURL: "https://codex.example", Label: "Codex team"}},
			OpenAICompatibility: []config.OpenAICompatibility{{
				Name:           "compat",
				BaseURL:        "https://compat.example",
				PricingCatalog: " ZAI-CODING-PLAN ",
				UsageProbe:     " ZAI ",
				APIKeyEntries:  []config.OpenAICompatibilityAPIKey{{APIKey: "compat", Label: "Compat team"}},
			}},
			VertexCompatAPIKey: []config.VertexCompatKey{{APIKey: "vertex", Label: "Vertex team"}},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, errSynthesize := NewConfigSynthesizer().Synthesize(ctx)
	if errSynthesize != nil {
		t.Fatalf("Synthesize() error = %v", errSynthesize)
	}
	want := map[string]string{
		"gemini":                   "Gemini team",
		"gemini-interactions":      "Interactions team",
		"claude":                   "Claude team",
		"codex":                    "Codex team",
		"openai-compatible-compat": "Compat team",
		"vertex":                   "Vertex team",
	}
	seen := make(map[string]bool, len(want))
	for _, auth := range auths {
		if expected, ok := want[auth.Provider]; ok {
			if auth.Label != expected || auth.Attributes["label"] != expected {
				t.Errorf("%s label/attribute = %q/%q, want %q", auth.Provider, auth.Label, auth.Attributes["label"], expected)
			}
			seen[auth.Provider] = true
		}
		if auth.Provider == "openai-compatible-compat" {
			if auth.Attributes["pricing_catalog"] != "zai-coding-plan" || auth.Attributes["usage_probe"] != "zai" {
				t.Errorf("compat selectors = %#v", auth.Attributes)
			}
		}
	}
	for provider := range want {
		if !seen[provider] {
			t.Errorf("missing synthesized provider %q", provider)
		}
	}
}

func TestFileSynthesizerPropagatesTopLevelLabel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.json")
	data, errMarshal := json.Marshal(map[string]any{
		"type":  "claude",
		"email": "email@example.com",
		"label": "  Billing account  ",
	})
	if errMarshal != nil {
		t.Fatalf("json.Marshal() error = %v", errMarshal)
	}
	auths, errSynthesize := SynthesizeAuthFile(&SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     filepath.Dir(path),
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}, path, data)
	if errSynthesize != nil {
		t.Fatalf("SynthesizeAuthFile() error = %v", errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("SynthesizeAuthFile() returned %d auths, want 1", len(auths))
	}
	if auths[0].Label != "  Billing account  " || auths[0].Attributes["label"] != "  Billing account  " {
		t.Fatalf("file label/attribute = %q/%q", auths[0].Label, auths[0].Attributes["label"])
	}
	if auths[0].Provider != "claude" {
		t.Fatalf("provider = %q, want claude", auths[0].Provider)
	}
}
