//go:build !windows

package storageusage

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func fileUsage(_ string, info os.FileInfo) (string, string, uint64) {
	stat := info.Sys().(*syscall.Stat_t)
	return fmt.Sprint(stat.Dev), fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), uint64(max(stat.Blocks, 0)) * 512
}

func inspectVolume(path string) (disk, error) {
	for {
		info, errStat := os.Stat(path)
		if os.IsNotExist(errStat) && filepath.Dir(path) != path {
			path = filepath.Dir(path)
			continue
		}
		if errStat != nil {
			return disk{}, errStat
		}
		var stat syscall.Statfs_t
		if err := syscall.Statfs(path, &stat); err != nil {
			return disk{}, err
		}
		device := info.Sys().(*syscall.Stat_t).Dev
		mount := path
		if !info.IsDir() {
			mount = filepath.Dir(mount)
		}
		for filepath.Dir(mount) != mount {
			parent := filepath.Dir(mount)
			parentInfo, errParent := os.Stat(parent)
			if errParent != nil || parentInfo.Sys().(*syscall.Stat_t).Dev != device {
				break
			}
			mount = parent
		}
		// Exclude filesystem-reserved blocks from usable capacity. Available is
		// space this process can actually use, rather than privileged free space.
		available := uint64(max(stat.Bavail, 0))
		reserved := uint64(stat.Bfree) - min(uint64(stat.Bfree), available)
		return disk{
			id: fmt.Sprint(device), path: mount,
			capacity:  (uint64(stat.Blocks) - min(uint64(stat.Blocks), reserved)) * uint64(stat.Bsize),
			available: available * uint64(stat.Bsize),
		}, nil
	}
}
