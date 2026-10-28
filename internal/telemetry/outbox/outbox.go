// Package outbox stores complete telemetry records for asynchronous ingestion.
package outbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
)

const directory = "telemetry-outbox"

var encoders sync.Pool
var decoders sync.Pool

// Write atomically publishes one compressed record under dir.
func Write(dir string, r *record.Request) error {
	if r == nil {
		return fmt.Errorf("outbox: nil record")
	}
	if r.RequestID == "" || strings.ContainsAny(r.RequestID, `/\\`) || r.RequestID == "." || r.RequestID == ".." {
		return fmt.Errorf("outbox: invalid request ID")
	}
	folder := filepath.Join(dir, directory)
	if err := os.MkdirAll(folder, 0700); err != nil {
		return fmt.Errorf("outbox: create directory: %w", err)
	}
	f, err := os.CreateTemp(folder, ".capture-*.tmp")
	if err != nil {
		return fmt.Errorf("outbox: create temporary file: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	encoder, _ := encoders.Get().(*zstd.Encoder)
	if encoder == nil {
		encoder, err = zstd.NewWriter(f)
		if err != nil {
			_ = f.Close()
			return fmt.Errorf("outbox: create encoder: %w", err)
		}
	} else {
		encoder.Reset(f)
	}
	err = json.NewEncoder(encoder).Encode(r)
	if closeErr := encoder.Close(); err == nil {
		err = closeErr
	}
	encoder.Reset(nil)
	encoders.Put(encoder)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("outbox: write record: %w", err)
	}
	timestamp := r.StartedAt.UnixNano()
	if r.StartedAt.IsZero() {
		timestamp = time.Now().UnixNano()
	}
	path := filepath.Join(folder, fmt.Sprintf("%d-%s.json.zst", timestamp, r.RequestID))
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("outbox: publish record: %w", err)
	}
	return nil
}

// List returns complete records in filename order.
func List(dir string) ([]string, error) {
	folder := filepath.Join(dir, directory)
	entries, err := os.ReadDir(folder)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("outbox: list: %w", err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json.zst") {
			paths = append(paths, filepath.Join(folder, entry.Name()))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// Read decodes a complete record.
func Read(path string) (*record.Request, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("outbox: open: %w", err)
	}
	defer f.Close()
	decoder, _ := decoders.Get().(*zstd.Decoder)
	if decoder == nil {
		decoder, err = zstd.NewReader(nil)
		if err != nil {
			return nil, fmt.Errorf("outbox: create decoder: %w", err)
		}
	}
	defer func() { _ = decoder.Reset(nil); decoders.Put(decoder) }()
	if err = decoder.Reset(f); err != nil {
		return nil, decodeError(err)
	}
	var r record.Request
	if err = json.NewDecoder(decoder).Decode(&r); err != nil {
		return nil, decodeError(err)
	}
	return &r, nil
}

// Quarantine preserves a rejected record without blocking later outbox files.
func Quarantine(path string) error {
	folder := filepath.Join(filepath.Dir(path), "failed")
	if err := os.MkdirAll(folder, 0700); err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(folder, filepath.Base(path)))
}

// CleanupTemps reclaims abandoned atomic writes. The age guard avoids touching
// another process's in-progress capture when it shares the log directory.
func CleanupTemps(dir string, now time.Time) (int, int64, error) {
	folder := filepath.Join(dir, directory)
	entries, err := os.ReadDir(folder)
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	var count int
	var size int64
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), ".capture-") || !strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return count, size, err
		}
		if now.Sub(info.ModTime()) < time.Hour {
			continue
		}
		if err = os.Remove(filepath.Join(folder, entry.Name())); err != nil && !os.IsNotExist(err) {
			return count, size, err
		}
		count++
		size += info.Size()
	}
	return count, size, nil
}

func decodeError(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("outbox: read: %w", err)
	}
	return fmt.Errorf("%w: outbox decode: %w", record.ErrInvalidRecord, err)
}
