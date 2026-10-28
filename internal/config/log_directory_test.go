package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRuntimeLogDirectorySurvivesClonesWithoutSerialization(t *testing.T) {
	cfg := &Config{RuntimeLogDirectory: "runtime-only-directory"}
	if got := cfg.CloneForRuntime().RuntimeLogDirectory; got != cfg.RuntimeLogDirectory {
		t.Fatalf("clone lost binding: %q", got)
	}
	encodedJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	encodedYAML, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, encoded := range [][]byte{encodedJSON, encodedYAML} {
		if strings.Contains(string(encoded), cfg.RuntimeLogDirectory) {
			t.Fatal("runtime path was serialized as user configuration")
		}
	}
}
