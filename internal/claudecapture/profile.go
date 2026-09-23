package claudecapture

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const schemaVersion = 1

var versionPattern = regexp.MustCompile(`\b[0-9]+\.[0-9]+\.[0-9]+\b`)
var exactVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
var bodyKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
var claudeCodeUserAgentPattern = regexp.MustCompile(`(?i)^claude-cli/[0-9]+\.[0-9]+\.[0-9]+\s+\(external,\s*[^,)]+(?:,\s*agent-sdk/[0-9]+\.[0-9]+\.[0-9]+)?\)$`)

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
	root, err := DefaultRoot()
	if err != nil {
		return "", err
	}
	reference, err := activeReference(root)
	if err != nil {
		return "", err
	}
	return reference.ProfilePath, nil
}

func InstalledVersion() (string, error) {
	root, err := DefaultRoot()
	if err != nil {
		return "", err
	}
	reference, err := activeReference(root)
	if err != nil {
		return "", err
	}
	return versionOfPrivate(reference.Executable, root)
}

func LoadCurrent() (*Profile, error) {
	root, err := DefaultRoot()
	if err != nil {
		return nil, err
	}
	reference, err := activeReference(root)
	if err != nil {
		return nil, err
	}
	version, err := versionOfPrivate(reference.Executable, root)
	if err != nil || version != reference.Version {
		return nil, errors.New("private Claude Code executable does not match its active reference")
	}
	return Load(reference.ProfilePath, version)
}

func Load(path, cliVersion string) (*Profile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect Claude Code OAuth reference: %w", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("Claude Code OAuth reference must be private to its owner")
	}
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
	hasMessages := false
	for _, variant := range p.Variants {
		if variant.Endpoint != "messages" && variant.Endpoint != "count_tokens" {
			return errors.New("Claude Code OAuth reference contains invalid endpoint")
		}
		if variant.Endpoint == "messages" {
			hasMessages = true
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
	if !hasMessages {
		return errors.New("Claude Code OAuth reference lacks a completed Messages request")
	}
	return nil
}

// CheckHeaders compares final outbound headers to the observed CLI request.
func (p *Profile) CheckHeaders(headers http.Header) error {
	if err := p.validate(p.ClaudeVersion); err != nil {
		return err
	}
	if !claudeCodeUserAgentPattern.MatchString(strings.TrimSpace(headers.Get("User-Agent"))) {
		return errors.New("Claude Code OAuth header User-Agent must match claude-cli/X.X.X")
	}
	for key, want := range p.Headers {
		if key == "Anthropic-Beta" || key == "User-Agent" || key == "X-Stainless-Package-Version" ||
			key == "X-Stainless-Runtime-Version" || key == "X-Stainless-OS" || key == "X-Stainless-Arch" {
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

// CheckRequest validates stable headers and Messages protocol fields. CLI,
// package, and runtime versions may change independently of the capture.
// Optional fields, betas, and streaming vary across genuine Claude Code requests.
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
