package auth

import "testing"

func TestQuotaRecoveryProviderForAuth(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		want string
	}{
		{name: "nil auth", auth: nil, want: ""},
		{
			name: "explicit usage probe overrides the adapter",
			auth: &Auth{Provider: "codex", Attributes: map[string]string{"usage_probe": "zai"}},
			want: "zai",
		},
		{
			name: "explicit usage probe overrides a named compatibility adapter",
			auth: &Auth{Provider: "openai-compatible-opencode-go", Attributes: map[string]string{"usage_probe": "zai"}},
			want: "zai",
		},
		{
			name: "explicit usage probe ignores case and spacing",
			auth: &Auth{Provider: "codex", Attributes: map[string]string{"usage_probe": " OpenCode-Go "}},
			want: "opencode-go",
		},
		{
			name: "unknown explicit usage probe stays unsupported instead of falling back to the adapter",
			auth: &Auth{Provider: "codex", Attributes: map[string]string{"usage_probe": "kimi"}},
			want: "",
		},
		{
			name: "adapter name is not an accepted usage probe value",
			auth: &Auth{Provider: "claude", Attributes: map[string]string{"usage_probe": "codex"}},
			want: "",
		},
		{
			name: "blank usage probe falls back to the adapter",
			auth: &Auth{Provider: "openai-compatible-z.ai", Attributes: map[string]string{"usage_probe": " "}},
			want: "zai",
		},
		{name: "z.ai provider name", auth: &Auth{Provider: "z.ai"}, want: "zai"},
		{name: "z.ai compatibility provider", auth: &Auth{Provider: "openai-compatible-z.ai"}, want: "zai"},
		{name: "opencode go compatibility provider", auth: &Auth{Provider: "openai-compatible-opencode-go"}, want: "opencode-go"},
		{name: "opencode go provider name", auth: &Auth{Provider: "OpenCode Go"}, want: "opencode-go"},
		{
			name: "synthesizer provider_key for a compat entry named OpenCode Go",
			auth: &Auth{Provider: "openai-compatible-opencode go"},
			want: "opencode-go",
		},
		{name: "z.ai compat entry with a descriptive name", auth: &Auth{Provider: "openai-compatible-zai-coding-plan"}, want: "zai"},
		{name: "codex provider passes through", auth: &Auth{Provider: "codex"}, want: "codex"},
		{name: "claude provider passes through", auth: &Auth{Provider: "claude"}, want: "claude"},
		{name: "kiro provider passes through", auth: &Auth{Provider: "kiro"}, want: "kiro"},
		{
			name: "generic compatibility provider uses provider_key",
			auth: &Auth{Provider: "openai-compatibility", Attributes: map[string]string{"provider_key": "openai-compatible-z.ai"}},
			want: "zai",
		},
		{
			name: "generic compatibility provider uses compat_name",
			auth: &Auth{Provider: "openai-compatibility", Attributes: map[string]string{"compat_name": "OpenCode Go"}},
			want: "opencode-go",
		},
		{name: "unrelated adapter has no identity", auth: &Auth{Provider: "gemini"}, want: ""},
		{name: "antigravity has no quota recovery probe", auth: &Auth{Provider: "antigravity"}, want: ""},
		{
			name: "model names are never provider names",
			auth: &Auth{
				Provider:   "custom",
				Attributes: map[string]string{"model": "z.ai", "models": "opencode-go", "base_url": "https://api.z.ai"},
				Metadata:   map[string]any{"model": "glm-5.3"},
			},
			want: "",
		},
		{
			name: "explicit probe wins even when the adapter is unknown",
			auth: &Auth{Provider: "gemini", Attributes: map[string]string{"usage_probe": "opencode-go"}},
			want: "opencode-go",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := QuotaRecoveryProviderForAuth(test.auth); got != test.want {
				t.Fatalf("QuotaRecoveryProviderForAuth() = %q, want %q", got, test.want)
			}
		})
	}
}
