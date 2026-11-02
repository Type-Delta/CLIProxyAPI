package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestV8APIKeyNamesRoundTrip(t *testing.T) {
	for _, family := range v8KeyFamilies {
		t.Run(family.current, func(t *testing.T) {
			raw := fmt.Sprintf(`config-version: 8
api-keys:
  %s:
    - name: configured-group
      base-url: https://api.example.invalid
      keys:
        - api-key: dummy-first
        - api-key: dummy-second
`, family.current)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"unchanged", "regenerated", "snapshot-collapse", "legacy-migration"} {
				t.Run(mode, func(t *testing.T) {
					var saved []byte
					switch mode {
					case "unchanged", "regenerated":
						// Changing the family forces restoreV8Layout to rebuild its groups.
						if mode == "regenerated" {
							switch family.current {
							case "gemini":
								cfg.GeminiKey[0].Priority++
							case "interactions":
								cfg.InteractionsKey[0].Priority++
							case "codex":
								cfg.CodexKey[0].Priority++
							case "xai":
								cfg.XAIKey[0].Priority++
							case "meta":
								cfg.MetaKey[0].Priority++
							case "claude":
								cfg.ClaudeKey[0].Priority++
							case "vertex":
								cfg.VertexCompatAPIKey[0].Priority++
							case "openai-compatibility":
								cfg.OpenAICompatibility[0].Priority++
							}
						}
						if err := SaveConfigPreserveComments(path, cfg, true); err != nil {
							t.Fatal(err)
						}
						saved, err = os.ReadFile(path)
					case "snapshot-collapse", "legacy-migration":
						var legacy []byte
						if mode == "snapshot-collapse" {
							legacy, err = yaml.Marshal(cfg)
						} else {
							legacy, err = yaml.Marshal((*legacyConfig)(cfg))
						}
						if err != nil {
							t.Fatal(err)
						}
						if mode == "legacy-migration" {
							if err := os.WriteFile(path, legacy, 0600); err != nil {
								t.Fatal(err)
							}
							loaded, errLoad := LoadConfig(path)
							if errLoad != nil {
								t.Fatal(errLoad)
							}
							if err := SaveConfigPreserveComments(path, loaded, true); err != nil {
								t.Fatal(err)
							}
							saved, err = os.ReadFile(path)
						} else {
							saved, _, err = NormalizeConfigLayout(legacy, true)
						}
					}
					if err != nil {
						t.Fatal(err)
					}
					if err := ValidateV8Config(saved); err != nil {
						t.Fatal(err)
					}
					var doc yaml.Node
					if err := yaml.Unmarshal(saved, &doc); err != nil {
						t.Fatal(err)
					}
					groups := yamlPath(doc.Content[0], "api-keys."+family.current)
					if groups == nil || len(groups.Content) == 0 {
						t.Fatal("lost API-key groups")
					}
					count := 0
					for _, group := range groups.Content {
						if name := yamlPath(group, "name"); name == nil || name.Value != "configured-group" {
							t.Fatal("lost configured group name")
						}
						for _, key := range yamlPath(group, "keys").Content {
							count++
							if yamlPath(key, "name") != nil {
								t.Fatal("group name leaked into key")
							}
						}
					}
					if count != 2 {
						t.Fatalf("got %d keys, want 2", count)
					}
					reloaded, err := ParseConfigBytes(saved)
					if err != nil {
						t.Fatal(err)
					}
					var node yaml.Node
					if err := node.Encode((*legacyConfig)(reloaded)); err != nil {
						t.Fatal(err)
					}
					for _, entry := range yamlPath(&node, family.old).Content {
						if name := yamlPath(entry, "name"); name == nil || name.Value != "configured-group" {
							t.Fatal("reloaded entry lost name")
						}
					}
				})
			}
		})
	}
}

func TestV8APIKeyNameOverrides(t *testing.T) {
	for _, family := range v8KeyFamilies {
		if family.current == "openai-compatibility" {
			continue
		}
		t.Run(family.current, func(t *testing.T) {
			raw := fmt.Sprintf(`config-version: 8
api-keys:
  %s:
    - name: group-name
      base-url: https://api.example.invalid
      keys:
        - api-key: dummy-inherited
        - api-key: dummy-override
          name: key-name
        - api-key: dummy-null
          name: null
        - api-key: dummy-empty
          name: ""
`, family.current)
			if err := ValidateV8Config([]byte(raw)); err != nil {
				t.Fatal(err)
			}
			cfg, err := ParseConfigBytes([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			var node yaml.Node
			if err := node.Encode((*legacyConfig)(cfg)); err != nil {
				t.Fatal(err)
			}
			entries := yamlPath(&node, family.old)
			if entries == nil || len(entries.Content) != 4 {
				t.Fatal("lost keys")
			}
			for i, want := range []string{"group-name", "key-name", "group-name", ""} {
				got := ""
				if name := yamlPath(entries.Content[i], "name"); name != nil {
					got = name.Value
				}
				if got != want {
					t.Fatalf("key %d name=%q, want %q", i, got, want)
				}
			}
		})
	}
}
