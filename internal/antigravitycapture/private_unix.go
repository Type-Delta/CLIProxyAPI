//go:build !windows

package antigravitycapture

import (
	"os"
	"syscall"
)

func securePrivateDirectory(path string) error { return os.Chmod(path, 0700) }

func ownsPrivateDirectory(_ string, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Uid) == uint64(os.Geteuid())
}
