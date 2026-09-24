package claudecapture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func privatePath(root, path string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil || !withinRoot(root, path) {
		return "", errors.New("path must stay under the CPA private reference root")
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	canonicalPath, err := filepath.EvalSymlinks(path)
	if err != nil || !withinRoot(canonicalRoot, canonicalPath) {
		return "", errors.New("path resolves outside the CPA private reference root")
	}
	return path, nil
}

func preparePrivateHome(dir string) error {
	for _, name := range []string{"config", "data", "cache", "state", "claude", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
			return fmt.Errorf("create private Claude %s directory: %w", name, err)
		}
	}
	return nil
}

func privateCommandEnvironment(dir, proxyAddress, caPath string) []string {
	commandPath := "/usr/bin:/bin"
	if nodePath, err := exec.LookPath("node"); err == nil {
		commandPath = filepath.Dir(nodePath) + string(os.PathListSeparator) + commandPath
	}
	env := []string{
		"PATH=" + commandPath,
		"HOME=" + dir,
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "config"),
		"XDG_DATA_HOME=" + filepath.Join(dir, "data"),
		"XDG_CACHE_HOME=" + filepath.Join(dir, "cache"),
		"XDG_STATE_HOME=" + filepath.Join(dir, "state"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(dir, "claude"),
		"TMPDIR=" + filepath.Join(dir, "tmp"),
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_AUTOUPDATER=1",
		"DISABLE_TELEMETRY=1",
		"NO_PROXY=",
	}
	for _, key := range []string{"LANG", "LC_ALL", "TZ"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	if proxyAddress != "" {
		env = append(env, "HTTPS_PROXY=http://"+proxyAddress, "HTTP_PROXY=http://"+proxyAddress, "ALL_PROXY=http://"+proxyAddress)
	}
	if caPath != "" {
		env = append(env, "NODE_EXTRA_CA_CERTS="+caPath)
	}
	return env
}

func versionOfPrivate(cli, root string) (string, error) {
	resolved, err := filepath.EvalSymlinks(cli)
	if err != nil {
		return "", fmt.Errorf("resolve private Claude executable: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil || !withinRoot(root, resolved) {
		return "", errors.New("Claude executable must be inside the CPA private reference directory")
	}
	home := filepath.Join(root, ".version-home")
	if err := preparePrivateHome(home); err != nil {
		return "", err
	}
	command := exec.CommandContext(context.Background(), cli, "--version")
	command.Dir = home
	command.Env = privateCommandEnvironment(home, "", "")
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("run private Claude Code version command: %w", err)
	}
	version := versionPattern.FindString(string(out))
	if version == "" {
		return "", errors.New("private Claude Code version command returned no version")
	}
	return version, nil
}

func runWithOAuthDescriptor(command *exec.Cmd, token string) error {
	reader, writer, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create private OAuth descriptor: %w", err)
	}
	defer reader.Close()
	defer writer.Close()
	command.ExtraFiles = []*os.File{reader}
	command.Env = append(command.Env, "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=3")
	if err := command.Start(); err != nil {
		return err
	}
	_ = reader.Close()
	_, errWrite := io.WriteString(writer, token+"\n")
	_ = writer.Close()
	if errWrite != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("send OAuth token to private Claude process: %w", errWrite)
	}
	return command.Wait()
}
