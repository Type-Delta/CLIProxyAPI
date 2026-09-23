package claudecapture

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const schemaVersion = 1

var versionPattern = regexp.MustCompile(`\b[0-9]+\.[0-9]+\.[0-9]+\b`)
var exactVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
var bodyKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

var allowedHeaders = map[string]bool{
	"User-Agent":                  true,
	"Anthropic-Version":           true,
	"Anthropic-Beta":              true,
	"X-Stainless-Lang":            true,
	"X-Stainless-Package-Version": true,
	"X-Stainless-OS":              true,
	"X-Stainless-Arch":            true,
	"X-Stainless-Runtime":         true,
	"X-Stainless-Runtime-Version": true,
}

// Profile contains only static wire headers and the names of top-level JSON fields.
// Request content, credentials, account IDs, and session IDs are never persisted.
type Profile struct {
	SchemaVersion int               `json:"schema_version"`
	ClaudeVersion string            `json:"claude_code_version"`
	CapturedAt    time.Time         `json:"captured_at"`
	Headers       map[string]string `json:"headers"`
	Variants      []Variant         `json:"variants"`
}

// Variant is one endpoint and request shape observed from the first-party CLI.
type Variant struct {
	Endpoint string   `json:"endpoint"`
	Stream   bool     `json:"stream"`
	Beta     string   `json:"anthropic_beta"`
	BodyKeys []string `json:"body_keys"`
}

func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(dir, "cli-proxy-api", "claude-code-oauth-profile.json"), nil
}

func InstalledVersion() (string, error) {
	return versionOf("claude")
}

func versionOf(cli string) (string, error) {
	out, err := exec.Command(cli, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("run Claude Code version command: %w", err)
	}
	version := versionPattern.FindString(string(out))
	if version == "" {
		return "", errors.New("Claude Code version command returned no version")
	}
	return version, nil
}

func LoadCurrent() (*Profile, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	version, err := InstalledVersion()
	if err != nil {
		return nil, err
	}
	return Load(path, version)
}

func Load(path, cliVersion string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Claude Code OAuth reference: %w", err)
	}
	var profile Profile
	if err := json.Unmarshal(data, &profile); err != nil {
		return nil, fmt.Errorf("decode Claude Code OAuth reference: %w", err)
	}
	if err := profile.validate(cliVersion); err != nil {
		return nil, err
	}
	return &profile, nil
}

func (p *Profile) validate(cliVersion string) error {
	if p == nil || p.SchemaVersion != schemaVersion || !exactVersionPattern.MatchString(p.ClaudeVersion) || p.ClaudeVersion != cliVersion || p.CapturedAt.IsZero() || p.CapturedAt.After(time.Now().Add(time.Minute)) {
		return errors.New("Claude Code OAuth reference is invalid or stale for the installed CLI version")
	}
	if len(p.Headers) == 0 || len(p.Variants) == 0 || p.Headers["User-Agent"] == "" || p.Headers["Anthropic-Version"] == "" {
		return errors.New("Claude Code OAuth reference has no complete Messages request")
	}
	for key, value := range p.Headers {
		if !allowedHeaders[key] || value == "" || strings.ContainsAny(value, "\r\n") || len(value) > 4096 {
			return fmt.Errorf("Claude Code OAuth reference contains an invalid static header %q", key)
		}
	}
	seen := make(map[string]bool)
	for _, variant := range p.Variants {
		if variant.Endpoint != "messages" && variant.Endpoint != "count_tokens" {
			return errors.New("Claude Code OAuth reference contains invalid endpoint")
		}
		if len(variant.BodyKeys) == 0 || len(variant.Beta) > 4096 || !hasBeta(variant.Beta, "oauth-2025-04-20") {
			return errors.New("Claude Code OAuth reference contains invalid request variant")
		}
		for _, key := range variant.BodyKeys {
			if len(key) > 80 || !bodyKeyPattern.MatchString(key) {
				return errors.New("Claude Code OAuth reference contains invalid body keys")
			}
		}
		name := variant.Endpoint + fmt.Sprint(variant.Stream) + variant.Beta + strings.Join(variant.BodyKeys, ",")
		if seen[name] {
			return errors.New("Claude Code OAuth reference contains duplicate variants")
		}
		seen[name] = true
	}
	return nil
}

// CheckHeaders compares final outbound headers to the observed CLI request.
func (p *Profile) CheckHeaders(headers http.Header) error {
	if err := p.validate(p.ClaudeVersion); err != nil {
		return err
	}
	for key, want := range p.Headers {
		if key == "Anthropic-Beta" {
			continue
		}
		if got := headers.Get(key); got != want {
			return fmt.Errorf("Claude Code OAuth header %s differs from the captured reference", key)
		}
	}
	if !hasBeta(headers.Get("Anthropic-Beta"), "oauth-2025-04-20") {
		return errors.New("Claude Code OAuth header Anthropic-Beta lacks oauth-2025-04-20")
	}
	return nil
}

// CheckRequest pins the captured software headers and validates the stable
// Messages protocol fields. Optional fields, betas, and streaming vary across
// genuine Claude Code requests, so a single probe cannot enumerate them.
func (p *Profile) CheckRequest(headers http.Header, body []byte, endpoint string) error {
	if err := p.CheckHeaders(headers); err != nil {
		return err
	}
	if endpoint != "messages" && endpoint != "count_tokens" {
		return errors.New("Claude Code OAuth request uses an unsupported endpoint")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return errors.New("Claude Code OAuth request body is not JSON")
	}
	if len(fields) == 0 {
		return errors.New("Claude Code OAuth request body is empty")
	}
	var model string
	if err := json.Unmarshal(fields["model"], &model); err != nil || model == "" {
		return errors.New("Claude Code OAuth request has invalid model")
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(fields["messages"], &messages); err != nil || messages == nil {
		return errors.New("Claude Code OAuth request has invalid messages")
	}
	if raw, ok := fields["stream"]; ok {
		var stream bool
		if err := json.Unmarshal(raw, &stream); err != nil {
			return errors.New("Claude Code OAuth request has invalid stream field")
		}
	}
	return nil
}

func hasBeta(value, beta string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.TrimSpace(part) == beta {
			return true
		}
	}
	return false
}

func writeProfile(path string, p *Profile) error {
	if err := p.validate(p.ClaudeVersion); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create private profile directory: %w", err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Claude Code OAuth reference: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".claude-code-oauth-*")
	if err != nil {
		return fmt.Errorf("create private temporary profile: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
