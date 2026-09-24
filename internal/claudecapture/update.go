package claudecapture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const updateInterval = 24 * time.Hour

var updateMu sync.Mutex

type privateReference struct {
	Version     string
	Executable  string
	ProfilePath string
}

type activeState struct {
	Version string `json:"version"`
}

type checkState struct {
	Version   string    `json:"latest_version"`
	CheckedAt time.Time `json:"checked_at"`
}

func DefaultRoot() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(dir, "cli-proxy-api", "claude-reference"), nil
}

// EnsureCurrent checks the official release at most once per 24 hours. It
// installs and captures in private staging before changing the active pointer.
// Call it from a background task or an explicit management command.
func EnsureCurrent(ctx context.Context, oauthToken string) (*Profile, error) {
	return ensureCurrent(ctx, oauthToken, false)
}

// UpdateNow forces a release check even if the last check was within 24 hours.
func UpdateNow(ctx context.Context, oauthToken string) (*Profile, error) {
	return ensureCurrent(ctx, oauthToken, true)
}

func ensureCurrent(ctx context.Context, oauthToken string, force bool) (*Profile, error) {
	if oauthToken == "" || len(oauthToken) > 8192 || strings.ContainsAny(oauthToken, "\r\n") {
		return nil, errors.New("Claude OAuth token is missing or invalid")
	}
	updateMu.Lock()
	defer updateMu.Unlock()
	root, err := DefaultRoot()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("create private Claude reference root: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(filepath.Join(root, "npm-cache")); err != nil {
			log.WithError(err).Warn("failed to remove private Claude npm cache")
		}
	}()
	current, errCurrent := LoadCurrent()
	state := readCheckState(root)
	if force || shouldCheck(time.Now(), state.CheckedAt) || !exactVersionPattern.MatchString(state.Version) {
		latest, err := lookupLatest(ctx, root)
		if err != nil {
			return nil, err
		}
		state = checkState{Version: latest, CheckedAt: time.Now().UTC()}
		if err := writePrivateJSON(filepath.Join(root, "last-check.json"), state); err != nil {
			return nil, err
		}
	}
	if errCurrent == nil && current.ClaudeVersion == state.Version {
		return current, nil
	}
	return installCapturePromote(ctx, root, state.Version, oauthToken)
}

func shouldCheck(now, last time.Time) bool {
	return last.IsZero() || last.After(now) || !now.Before(last.Add(updateInterval))
}

func readCheckState(root string) checkState {
	data, err := os.ReadFile(filepath.Join(root, "last-check.json"))
	if err != nil {
		return checkState{}
	}
	var state checkState
	if json.Unmarshal(data, &state) != nil || !exactVersionPattern.MatchString(state.Version) {
		return checkState{}
	}
	return state
}

func activeReference(root string) (privateReference, error) {
	data, err := os.ReadFile(filepath.Join(root, "active.json"))
	if err != nil {
		return privateReference{}, fmt.Errorf("private Claude Code reference is unavailable: %w", err)
	}
	var state activeState
	if json.Unmarshal(data, &state) != nil || !exactVersionPattern.MatchString(state.Version) {
		return privateReference{}, errors.New("private Claude Code active reference is invalid")
	}
	base := filepath.Join(root, "versions", state.Version)
	return privateReference{Version: state.Version, Executable: filepath.Join(base, "node_modules", ".bin", "claude"), ProfilePath: filepath.Join(base, "oauth-profile.json")}, nil
}

func lookupLatest(ctx context.Context, root string) (string, error) {
	command, err := privateNPMCommand(ctx, root, "view", "@anthropic-ai/claude-code", "version", "--json")
	if err != nil {
		return "", err
	}
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("check official Claude Code version: %w", err)
	}
	version := strings.Trim(strings.TrimSpace(string(out)), `"`)
	if !exactVersionPattern.MatchString(version) {
		return "", errors.New("official Claude Code registry returned an invalid version")
	}
	return version, nil
}

func privateNPMCommand(ctx context.Context, root string, args ...string) (*exec.Cmd, error) {
	home := filepath.Join(root, ".npm-home")
	if err := preparePrivateHome(home); err != nil {
		return nil, err
	}
	cache := filepath.Join(root, "npm-cache")
	if err := os.MkdirAll(cache, 0700); err != nil {
		return nil, err
	}
	npmPath, err := exec.LookPath("npm")
	if err != nil {
		return nil, fmt.Errorf("find npm for private Claude Code installation: %w", err)
	}
	command := exec.CommandContext(ctx, npmPath, args...)
	command.Dir = home
	command.Env = append(privateCommandEnvironment(home, "", ""), "NPM_CONFIG_CACHE="+cache, "NPM_CONFIG_USERCONFIG="+os.DevNull)
	command.Stderr = io.Discard
	return command, nil
}

func installCapturePromote(ctx context.Context, root, version, token string) (*Profile, error) {
	if !exactVersionPattern.MatchString(version) {
		return nil, errors.New("cannot install an invalid Claude Code version")
	}
	previous, _ := activeReference(root)
	stage, err := os.MkdirTemp(root, ".stage-")
	if err != nil {
		return nil, fmt.Errorf("create private Claude Code staging directory: %w", err)
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0700); err != nil {
		return nil, err
	}
	command, err := privateNPMCommand(ctx, root, "install", "--prefix", stage, "--no-audit", "--no-fund", "@anthropic-ai/claude-code@"+version)
	if err != nil {
		return nil, err
	}
	command.Stdout = io.Discard
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("install official Claude Code privately: %w", err)
	}
	cli := filepath.Join(stage, "node_modules", ".bin", "claude")
	installedVersion, err := versionOfPrivate(cli, root)
	if err != nil || installedVersion != version {
		return nil, errors.New("staged Claude Code executable does not match registry version")
	}
	profilePath := filepath.Join(stage, "oauth-profile.json")
	if err := CapturePrivate(ctx, CaptureOptions{Executable: cli, OAuthToken: token, OutputPath: profilePath, StateDir: root}); err != nil {
		return nil, fmt.Errorf("capture staged Claude Code OAuth reference: %w", err)
	}
	profile, err := Load(profilePath, version)
	if err != nil {
		return nil, fmt.Errorf("validate staged Claude Code OAuth reference: %w", err)
	}
	versions := filepath.Join(root, "versions")
	if err := os.MkdirAll(versions, 0700); err != nil {
		return nil, err
	}
	final := filepath.Join(versions, version)
	backup := ""
	if _, err := os.Stat(final); err == nil {
		backup, err = os.MkdirTemp(versions, ".previous-")
		if err != nil {
			return nil, err
		}
		if err := os.Remove(backup); err != nil {
			return nil, err
		}
		if err := os.Rename(final, backup); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.Rename(stage, final); err != nil {
		if backup != "" {
			_ = os.Rename(backup, final)
		}
		return nil, fmt.Errorf("publish private Claude Code version: %w", err)
	}
	if err := writePrivateJSON(filepath.Join(root, "active.json"), activeState{Version: version}); err != nil {
		return nil, fmt.Errorf("activate private Claude Code reference: %w", err)
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	if err := pruneOldVersions(versions, version, previous.Version); err != nil {
		log.WithError(err).Warn("failed to prune old private Claude Code versions")
	}
	return profile, nil
}

func pruneOldVersions(versions, active, previous string) error {
	entries, err := os.ReadDir(versions)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !exactVersionPattern.MatchString(entry.Name()) || entry.Name() == active || entry.Name() == previous {
			continue
		}
		if err := os.RemoveAll(filepath.Join(versions, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func writePrivateJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-")
	if err != nil {
		return err
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
