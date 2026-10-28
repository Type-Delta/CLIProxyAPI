package legacylog

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clickhouse"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/record"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/telemetry/store"
	log "github.com/sirupsen/logrus"
)

// Sink runs the normal synchronous telemetry ingestion pipeline.
type Sink interface {
	IngestRecords(context.Context, []*record.Request) error
}

type checkpoint struct {
	Last      string    `json:"last,omitempty"`
	LastAt    time.Time `json:"last_at,omitempty"`
	FilesSeen int       `json:"files_seen"`
	Imported  int       `json:"imported"`
	Skipped   int       `json:"skipped_unparseable"`
	Records   int       `json:"records"`
	Error     string    `json:"error,omitempty"`
	Attempts  int       `json:"attempts"`
}

var logFilename = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}T\d{6}(?:_\d+)?-[^-]+\.log(?:\.gz)?$`)

type sourceFile struct {
	name string
	at   time.Time
}

// RunJob imports only when the telemetry migration created the job. Each
// checkpoint follows one synchronous record ingestion. A crash between them
// replays the same deterministic request ID and timestamp.
func RunJob(ctx context.Context, db *clickhouse.DB, sink Sink, logsDir string) error {
	job, found, err := db.Job(ctx, store.LegacyJob)
	if err != nil || !found || job.State == "done" {
		return err
	}
	if job.State != "pending" && job.State != "running" && job.State != "failed" {
		return nil
	}
	var cp checkpoint
	if job.Detail != "" {
		if err = json.Unmarshal([]byte(job.Detail), &cp); err != nil {
			return fmt.Errorf("legacy import checkpoint: %w", err)
		}
	}
	cp.Last = sourceIdentity(cp.Last)
	cp.Error = ""
	save := func(state string) error {
		b, e := json.Marshal(cp)
		if e != nil {
			return e
		}
		return db.SetJob(ctx, store.LegacyJob, state, string(b))
	}
	fail := func(e error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cp.Error = e.Error()
		if saveErr := save("failed"); saveErr != nil {
			return errors.Join(e, saveErr)
		}
		return e
	}
	if err = save("running"); err != nil {
		return err
	}
	entries, err := os.ReadDir(logsDir)
	if os.IsNotExist(err) {
		return save("done")
	}
	if err != nil {
		return fail(err)
	}
	var files []sourceFile
	seenSources := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || strings.HasPrefix(name, "main.log") || !logFilename.MatchString(name) {
			continue
		}
		identity := sourceIdentity(name)
		if seenSources[identity] {
			continue
		}
		seenSources[identity] = true
		info, e := entry.Info()
		if e != nil {
			return fail(e)
		}
		at := info.ModTime()
		timestamp, e := sourceTimestamp(filepath.Join(logsDir, name))
		if e == nil {
			at = timestamp
		} else if !isCorrupt(e) {
			return fail(fmt.Errorf("legacy timestamp %s: %w", name, e))
		}
		files = append(files, sourceFile{name, at})
	}
	sort.Slice(files, func(a, b int) bool {
		if files[a].at.Equal(files[b].at) {
			return sourceIdentity(files[a].name) < sourceIdentity(files[b].name)
		}
		return files[a].at.Before(files[b].at)
	})
	start := 0
	// Upgrade old filename-only checkpoints while that source still exists.
	if cp.Last != "" && cp.LastAt.IsZero() {
		for _, f := range files {
			if sourceIdentity(f.name) == cp.Last {
				cp.LastAt = f.at
				break
			}
		}
	}
	if !cp.LastAt.IsZero() {
		start = sort.Search(len(files), func(n int) bool {
			return files[n].at.After(cp.LastAt) || files[n].at.Equal(cp.LastAt) && sourceIdentity(files[n].name) > cp.Last
		})
	}

	for _, f := range files[start:] {
		if err = ctx.Err(); err != nil {
			return err
		}
		data, e := readSource(filepath.Join(logsDir, f.name))
		if e != nil && !isCorrupt(e) {
			return fail(e)
		}
		var r *record.Request
		if e == nil {
			r, e = Parse(f.name, data)
		}
		if e != nil {
			cp.Attempts++
			log.WithError(e).Warnf("telemetry: skipped invalid legacy file after 1 decode attempt: %s", f.name)
			cp.Skipped++
		} else {
			attempt := 0
			dataAttempts := 0
			for {
				attempt++
				cp.Attempts++
				e = sink.IngestRecords(ctx, []*record.Request{r})
				if e == nil {
					break
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				permanent := record.DataError(e)
				if permanent {
					dataAttempts++
				} else {
					dataAttempts = 0
				}
				log.WithError(e).Warnf("telemetry: legacy attempt %d for %s failed (deterministic_data=%t)", attempt, f.name, permanent)
				cp.Error = e.Error()
				if permanent && dataAttempts >= 3 {
					log.WithError(e).Warnf("telemetry: skipped rejected legacy record after %d attempts: %s", attempt, f.name)
					cp.Skipped++
					break
				}
				if errSave := save("running"); errSave != nil {
					return errSave
				}
				timer := time.NewTimer(time.Duration(min(attempt, 30)) * time.Second)
				select {
				case <-ctx.Done():
					if !timer.Stop() {
						<-timer.C
					}
					return ctx.Err()
				case <-timer.C:
				}
			}
			if e == nil {
				cp.Error = ""
				cp.Imported++
				cp.Records++
			}
		}
		cp.FilesSeen++
		cp.Last = sourceIdentity(f.name)
		cp.LastAt = f.at
		if err = save("running"); err != nil {
			return err
		}
	}
	return save("done")
}

type corruptError struct{ error }

func isCorrupt(err error) bool { var e corruptError; return errors.As(err, &e) }
func readSource(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var reader io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		g, e := gzip.NewReader(f)
		if e != nil {
			return nil, sourceReadError(e)
		}
		defer g.Close()
		reader = g
	}
	b, err := io.ReadAll(reader)
	if err != nil && strings.HasSuffix(path, ".gz") {
		return nil, sourceReadError(err)
	}
	return b, err
}

// sourceTimestamp reads only the bounded request-info header, including gzip
// sources. Sorting never decompresses the full transcript a second time.
func sourceTimestamp(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	var reader io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		g, err := gzip.NewReader(f)
		if err != nil {
			return time.Time{}, sourceReadError(err)
		}
		defer g.Close()
		reader = g
	}
	scanner := bufio.NewScanner(io.LimitReader(reader, 16*1024))
	if !scanner.Scan() || scanner.Text() != "=== REQUEST INFO ===" {
		if err := scanner.Err(); err != nil {
			return time.Time{}, sourceReadError(err)
		}
		return time.Time{}, corruptError{fmt.Errorf("missing request header")}
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "=== HEADERS ===" {
			break
		}
		if strings.HasPrefix(line, "Timestamp: ") {
			at, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(line, "Timestamp: "))
			if err != nil {
				return time.Time{}, corruptError{err}
			}
			return at, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return time.Time{}, sourceReadError(err)
	}
	return time.Time{}, corruptError{fmt.Errorf("missing timestamp")}
}

func sourceReadError(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, gzip.ErrHeader) || errors.Is(err, gzip.ErrChecksum) {
		return corruptError{err}
	}
	return err
}
