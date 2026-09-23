package claudecapture

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const sandboxRoot = "/cpa"

func sandboxPath(root, hostPath string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	hostPath, err = filepath.Abs(hostPath)
	if err != nil || !withinRoot(root, hostPath) {
		return "", fmt.Errorf("path must stay under the CPA private reference root")
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	canonicalPath, err := filepath.EvalSymlinks(hostPath)
	if err != nil || !withinRoot(canonicalRoot, canonicalPath) {
		return "", fmt.Errorf("path resolves outside the CPA private reference root")
	}
	rel, _ := filepath.Rel(root, hostPath)
	return filepath.ToSlash(filepath.Join(sandboxRoot, rel)), nil
}

// sandboxCommand exposes /usr and /etc read-only, creates a private /tmp, and
// binds only the CPA reference root. The real home is absent from the namespace.
func sandboxCommand(ctx context.Context, root, workdir, executable string, writable []string, args ...string) (*exec.Cmd, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("private Claude reference requires Linux bubblewrap isolation")
	}
	insideWorkdir, err := sandboxPath(root, workdir)
	if err != nil {
		return nil, err
	}
	insideExecutable := executable
	if executable != "/usr/bin/npm" {
		insideExecutable, err = sandboxPath(root, executable)
		if err != nil {
			return nil, err
		}
	}
	bwrapArgs := []string{
		"--die-with-parent",
		"--unshare-pid",
		"--unshare-ipc",
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/lib", "/lib",
		"--symlink", "usr/lib64", "/lib64",
		"--symlink", "usr/bin", "/bin",
		"--ro-bind", "/etc", "/etc",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--ro-bind", root, sandboxRoot,
	}
	for _, path := range writable {
		insidePath, err := sandboxPath(root, path)
		if err != nil {
			return nil, err
		}
		bwrapArgs = append(bwrapArgs, "--bind", path, insidePath)
	}
	bwrapArgs = append(bwrapArgs, "--chdir", insideWorkdir, "--", insideExecutable)
	bwrapArgs = append(bwrapArgs, args...)
	command := exec.CommandContext(ctx, "/usr/bin/bwrap", bwrapArgs...)
	command.Dir = root
	command.Env = []string{"PATH=/usr/bin", "HOME=/cpa", "NO_PROXY="}
	return command, nil
}

func sandboxAvailable() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("private Claude reference requires Linux bubblewrap isolation")
	}
	if _, err := os.Stat("/usr/bin/bwrap"); err != nil {
		return fmt.Errorf("private Claude reference requires /usr/bin/bwrap: %w", err)
	}
	return nil
}
