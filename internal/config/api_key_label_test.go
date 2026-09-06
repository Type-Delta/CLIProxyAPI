package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPIKeyLabelsRoundTripAndValidateOnLoad(t *testing.T) {
	label := "  团队 🚀 / 東京  "
	data := []byte("api-keys:\n  - key: first-key-with-enough-entropy\n    label: \"  团队 🚀 / 東京  \"\n  - second-key-with-enough-entropy\n")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if got := loaded.APIKeys[0].Label; got != label {
		t.Fatalf("loaded label = %q, want %q", got, label)
	}

	duplicate := []byte("api-keys:\n  - key: first-key-with-enough-entropy\n    label: same\n  - key: second-key-with-enough-entropy\n    label: same\n")
	if err := os.WriteFile(path, duplicate, 0o600); err != nil {
		t.Fatalf("write duplicate config: %v", err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "duplicate api key label") {
		t.Fatalf("LoadConfig() duplicate error = %v", err)
	}
	if _, err := ParseConfigBytes(duplicate); err == nil || !strings.Contains(err.Error(), "duplicate api key label") {
		t.Fatalf("ParseConfigBytes() duplicate error = %v", err)
	}
}
