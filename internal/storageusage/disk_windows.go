package storageusage

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func fileUsage(path string, info os.FileInfo) (string, string, uint64) {
	// Windows file allocation and identity are queried through the file handle
	// when available. Fall back to logical size if the file is being rotated.
	device := strings.ToUpper(filepath.VolumeName(path))
	identity := strings.ToLower(filepath.Clean(path))
	size := uint64(max(info.Size(), 0))
	if info.IsDir() {
		size = 0
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return device, identity, size
	}
	handle, errOpen := windows.CreateFile(pointer, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if errOpen == nil {
		var stat windows.ByHandleFileInformation
		if windows.GetFileInformationByHandle(handle, &stat) == nil {
			device = fmt.Sprint(stat.VolumeSerialNumber)
			identity = fmt.Sprintf("%d:%d:%d", stat.VolumeSerialNumber, stat.FileIndexHigh, stat.FileIndexLow)
		}
		var standard [24]byte
		if windows.GetFileInformationByHandleEx(handle, windows.FileStandardInfo, &standard[0], uint32(len(standard))) == nil {
			size = binary.LittleEndian.Uint64(standard[:8])
		}
		_ = windows.CloseHandle(handle)
	}
	return device, identity, size
}

func inspectVolume(path string) (disk, error) {
	for {
		_, errStat := os.Stat(path)
		if os.IsNotExist(errStat) && filepath.Dir(path) != path {
			path = filepath.Dir(path)
			continue
		}
		if errStat != nil {
			return disk{}, errStat
		}
		info, errInfo := os.Stat(path)
		if errInfo != nil {
			return disk{}, errInfo
		}
		if !info.IsDir() {
			path = filepath.Dir(path)
		}
		pointer, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return disk{}, err
		}
		var available, total uint64
		if err := windows.GetDiskFreeSpaceEx(pointer, &available, &total, nil); err != nil {
			return disk{}, err
		}
		var mount [windows.MAX_PATH + 1]uint16
		if err := windows.GetVolumePathName(pointer, &mount[0], uint32(len(mount))); err != nil {
			return disk{}, err
		}
		var serial uint32
		if err := windows.GetVolumeInformation(&mount[0], nil, 0, &serial, nil, nil, nil, 0); err != nil {
			return disk{}, err
		}
		return disk{id: fmt.Sprint(serial), path: windows.UTF16ToString(mount[:]), capacity: total, available: min(total, available)}, nil
	}
}
