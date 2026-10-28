// Package storageusage measures local CPA files against their backing filesystems.
package storageusage

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type Location struct {
	Path     string `json:"path"`
	Category string `json:"category"`
}

type Volume struct {
	Path           string     `json:"path"`
	CapacityBytes  uint64     `json:"capacity_bytes"`
	AvailableBytes uint64     `json:"available_bytes"`
	LogsBytes      uint64     `json:"logs_bytes"`
	DataBytes      uint64     `json:"data_bytes"`
	OtherBytes     uint64     `json:"other_bytes"`
	Locations      []Location `json:"locations"`
}

type Snapshot struct {
	MeasuredAt time.Time          `json:"measured_at"`
	Partial    bool               `json:"partial"`
	Volumes    []Volume           `json:"volumes"`
	ClickHouse *ClickHouseStorage `json:"clickhouse,omitempty"`
}

// Meter coalesces scans and caches a snapshot briefly to bound directory walks.
// Changing the configured locations invalidates the cached snapshot.
type Meter struct {
	mu         sync.Mutex
	locations  []Location
	snapshot   Snapshot
	clickHouse clickHouseCacheEntry
}

func (m *Meter) Measure(ctx context.Context, locations []Location) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if slices.Equal(m.locations, locations) && time.Since(m.snapshot.MeasuredAt) < 15*time.Second {
		return m.snapshot
	}
	snapshot := measure(ctx, locations, inspectVolume)
	if ctx.Err() == nil {
		m.locations = slices.Clone(locations)
		m.snapshot = snapshot
	}
	return snapshot
}

type disk struct {
	id, path            string
	capacity, available uint64
}

func measure(ctx context.Context, locations []Location, inspect func(string) (disk, error)) Snapshot {
	result := Snapshot{Volumes: []Volume{}}
	roots := make([]Location, 0, len(locations))
	for _, location := range locations {
		if strings.TrimSpace(location.Path) == "" {
			continue
		}
		absolute, errAbs := filepath.Abs(location.Path)
		if errAbs != nil {
			result.Partial = true
			continue
		}
		// Resolve configured symlinks, including the existing prefix of paths that
		// have not been created. WalkDir never follows symlinks inside these roots.
		existing := absolute
		for {
			_, errStat := os.Stat(existing)
			if !errors.Is(errStat, fs.ErrNotExist) || filepath.Dir(existing) == existing {
				break
			}
			existing = filepath.Dir(existing)
		}
		resolved, errResolve := filepath.EvalSymlinks(existing)
		if errResolve != nil {
			result.Partial = true
			continue
		}
		relative, errRel := filepath.Rel(existing, absolute)
		if errRel != nil {
			result.Partial = true
			continue
		}
		location.Path = filepath.Join(resolved, relative)
		if !slices.Contains(roots, location) {
			roots = append(roots, location)
		}
	}

	volumeIndex := make(map[string]int)
	addVolume := func(path string, location Location) (int, error) {
		value, errInspect := inspect(path)
		if errInspect != nil {
			return 0, errInspect
		}
		index, exists := volumeIndex[value.id]
		if !exists {
			index = len(result.Volumes)
			volumeIndex[value.id] = index
			result.Volumes = append(result.Volumes, Volume{
				Path: value.path, CapacityBytes: value.capacity, AvailableBytes: value.available,
				Locations: []Location{},
			})
		}
		volume := &result.Volumes[index]
		if !slices.Contains(volume.Locations, location) {
			volume.Locations = append(volume.Locations, location)
		}
		return index, nil
	}
	seen := make(map[string]struct{})
	for _, root := range roots {
		if ctx.Err() != nil {
			result.Partial = true
			break
		}
		if _, errVolume := addVolume(root.Path, root); errVolume != nil {
			result.Partial = true
			continue
		}
		errWalk := filepath.WalkDir(root.Path, func(path string, entry fs.DirEntry, errWalk error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if errWalk != nil {
				// Missing roots and files removed by rotation are normal.
				if !errors.Is(errWalk, fs.ErrNotExist) {
					result.Partial = true
				}
				return nil
			}
			info, errInfo := entry.Info()
			if errInfo != nil {
				if !errors.Is(errInfo, fs.ErrNotExist) {
					result.Partial = true
				}
				return nil
			}
			device, identity, size := fileUsage(path, info)
			if _, exists := seen[identity]; exists {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			seen[identity] = struct{}{}
			location := root
			for _, candidate := range roots {
				if len(candidate.Path) > len(location.Path) && within(path, candidate.Path) {
					location = candidate
				}
			}
			index, exists := volumeIndex[device]
			if !exists {
				var errVolume error
				index, errVolume = addVolume(path, location)
				if errVolume != nil {
					result.Partial = true
					return nil
				}
			}
			if location.Category == "logs" {
				result.Volumes[index].LogsBytes += size
			} else {
				result.Volumes[index].DataBytes += size
			}
			return nil
		})
		if errWalk != nil {
			result.Partial = true
		}
	}
	for index := range result.Volumes {
		volume := &result.Volumes[index]
		used := volume.CapacityBytes - min(volume.CapacityBytes, volume.AvailableBytes)
		// Files can grow during the scan. Keep the partition consistent with the
		// filesystem snapshot even when it changes underneath us.
		if volume.LogsBytes > used || volume.DataBytes > used-min(used, volume.LogsBytes) {
			result.Partial = true
			volume.LogsBytes = min(volume.LogsBytes, used)
			volume.DataBytes = min(volume.DataBytes, used-volume.LogsBytes)
		}
		volume.OtherBytes = used - volume.LogsBytes - volume.DataBytes
	}
	result.MeasuredAt = time.Now().UTC()
	return result
}

func within(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
