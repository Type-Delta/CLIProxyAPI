//go:build windows

package store

import "golang.org/x/sys/windows"

func availableBytes(path string) (uint64, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(pathPointer, &available, nil, nil); err != nil {
		return 0, err
	}
	return available, nil
}
