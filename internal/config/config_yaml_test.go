package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveConfigPreserveCommentsUpdateNestedScalar_NormalizesCRLFComments(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	input := strings.Join([]string{
		"# first comment",
		"# second comment",
		"remote-management:",
		"  # secret comment",
		"  secret-key: old",
		"port: 8317",
		"",
	}, "\r\n")
	if errWrite := os.WriteFile(configPath, []byte(input), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}

	if errUpdate := SaveConfigPreserveCommentsUpdateNestedScalar(configPath, []string{"remote-management", "secret-key"}, "new"); errUpdate != nil {
		t.Fatalf("SaveConfigPreserveCommentsUpdateNestedScalar() error = %v", errUpdate)
	}

	got, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatalf("read config: %v", errRead)
	}
	if bytes.Contains(got, []byte{'\r'}) {
		t.Fatalf("output contains carriage returns: %q", got)
	}
	if strings.Contains(string(got), "# first comment\n\n# second comment") {
		t.Fatalf("top-level comments gained a blank line: %q", got)
	}
	if strings.Contains(string(got), "# secret comment\n\n  secret-key") {
		t.Fatalf("nested comment gained a blank line: %q", got)
	}
	want := "# first comment\n# second comment\nremote-management:\n# secret comment\n  secret-key: new\nport: 8317\n"
	if string(got) != want {
		t.Fatalf("unexpected output\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestSaveConfigPreserveComments_NormalizesCRLFComments(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	input := "# first comment\r\n# second comment\r\ndebug: true\r\n"
	if errWrite := os.WriteFile(configPath, []byte(input), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}

	if errSave := SaveConfigPreserveComments(configPath, &Config{Debug: true}); errSave != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", errSave)
	}

	got, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatalf("read config: %v", errRead)
	}
	if bytes.Contains(got, []byte{'\r'}) {
		t.Fatalf("output contains carriage returns: %q", got)
	}
	if !bytes.HasPrefix(got, []byte("# first comment\n# second comment\n")) {
		t.Fatalf("top-level comments were not preserved adjacently: %q", got)
	}
}
