package claudecapture

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// StagedReference describes a privately installed, captured Claude Code
// candidate. Staging does not replace the system CLI or the active profile.
type StagedReference struct {
	Version     string
	Executable  string
	ProfilePath string
}

// StageLatest installs the latest official npm package in a private directory,
// captures an OAuth probe with that executable, and publishes the directory
// only after the probe succeeds. It never modifies the system CLI.
func StageLatest(ctx context.Context) (*StagedReference, error) {
	dataDir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("find user config directory: %w", err)
	}
	root := filepath.Join(dataDir, "cli-proxy-api", "claude-reference")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("create private Claude reference directory: %w", err)
	}
	latestCommand := exec.CommandContext(ctx, "npm", "view", "@anthropic-ai/claude-code", "version", "--json")
	latestOutput, err := latestCommand.Output()
	if err != nil {
		return nil, fmt.Errorf("check latest official Claude Code version: %w", err)
	}
	latest := strings.Trim(strings.TrimSpace(string(latestOutput)), `"`)
	if !exactVersionPattern.MatchString(latest) {
		return nil, fmt.Errorf("npm returned an invalid Claude Code version")
	}
	final := filepath.Join(root, latest)
	if candidate, ok := stagedReference(final, latest); ok {
		return candidate, nil
	}
	stage, err := os.MkdirTemp(root, ".stage-")
	if err != nil {
		return nil, fmt.Errorf("create Claude reference staging directory: %w", err)
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0700); err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, "npm", "install", "--prefix", stage, "--no-audit", "--no-fund", "@anthropic-ai/claude-code@latest")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("install official Claude Code package privately: %w", err)
	}
	cli := filepath.Join(stage, "node_modules", ".bin", "claude")
	version, err := versionOf(cli)
	if err != nil {
		return nil, fmt.Errorf("verify staged Claude Code: %w", err)
	}
	profilePath := filepath.Join(stage, "oauth-profile.json")
	if err := Capture(ctx, cli, profilePath); err != nil {
		return nil, fmt.Errorf("capture staged Claude Code OAuth reference: %w", err)
	}
	if _, err := Load(profilePath, version); err != nil {
		return nil, fmt.Errorf("validate staged Claude Code OAuth reference: %w", err)
	}
	final = filepath.Join(root, version)
	if _, err := os.Stat(final); err == nil {
		backup, err := os.MkdirTemp(root, ".previous-")
		if err != nil {
			return nil, fmt.Errorf("reserve previous Claude reference path: %w", err)
		}
		if err := os.Remove(backup); err != nil {
			return nil, err
		}
		if err := os.Rename(final, backup); err != nil {
			return nil, fmt.Errorf("move previous Claude reference aside: %w", err)
		}
		if err := os.Rename(stage, final); err != nil {
			_ = os.Rename(backup, final)
			return nil, fmt.Errorf("publish staged Claude reference: %w", err)
		}
		_ = os.RemoveAll(backup)
		return &StagedReference{Version: version, Executable: filepath.Join(final, "node_modules", ".bin", "claude"), ProfilePath: filepath.Join(final, "oauth-profile.json")}, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect existing Claude reference: %w", err)
	}
	if err := os.Rename(stage, final); err != nil {
		return nil, fmt.Errorf("publish staged Claude reference: %w", err)
	}
	return &StagedReference{Version: version, Executable: filepath.Join(final, "node_modules", ".bin", "claude"), ProfilePath: filepath.Join(final, "oauth-profile.json")}, nil
}

func stagedReference(dir, version string) (*StagedReference, bool) {
	cli := filepath.Join(dir, "node_modules", ".bin", "claude")
	profilePath := filepath.Join(dir, "oauth-profile.json")
	gotVersion, err := versionOf(cli)
	if err != nil || gotVersion != version {
		return nil, false
	}
	if _, err := Load(profilePath, version); err != nil {
		return nil, false
	}
	return &StagedReference{Version: version, Executable: cli, ProfilePath: profilePath}, true
}
