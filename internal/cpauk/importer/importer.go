package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/store"
)

type SourceRow struct {
	Offset int64
	Value  any
}

type Source interface {
	Kind() string
	Fingerprint(context.Context) (string, error)
	Read(context.Context, int64, int) ([]SourceRow, bool, error)
	Close() error
}

type Transformer func(context.Context, SourceRow) (model.Event, bool, error)

type Destination interface {
	WriteImportBatch(context.Context, []model.Event, string) (int64, error)
	LoadImportCheckpoint(context.Context, string) ([]byte, bool, error)
	SaveImportCheckpoint(context.Context, string, string, string, int64, int, []byte, bool, [5]int64) error
}

type Options struct {
	BatchID   string
	DryRun    bool
	ChunkSize int
	Resume    bool
}

type Importer struct {
	Destination Destination
	Transform   Transformer
}

type backupDestination interface {
	Backup(context.Context, string) (store.BackupManifest, error)
}

func (i *Importer) RunWithBackup(ctx context.Context, source Source, options Options, backupPath string) (model.ImportResult, error) {
	if !options.DryRun {
		if backupPath == "" {
			return model.ImportResult{}, fmt.Errorf("a pre-import backup path is required")
		}
		backupStore, ok := i.Destination.(backupDestination)
		if !ok {
			return model.ImportResult{}, fmt.Errorf("import destination does not support verified backup")
		}
		if _, err := backupStore.Backup(ctx, backupPath); err != nil {
			return model.ImportResult{}, fmt.Errorf("back up analytics before import: %w", err)
		}
	}
	return i.Run(ctx, source, options)
}

type checkpoint struct {
	Version     int      `json:"version"`
	BatchID     string   `json:"batch_id"`
	SourceKind  string   `json:"source_kind"`
	Fingerprint string   `json:"source_fingerprint"`
	Offset      int64    `json:"offset"`
	Chunk       int      `json:"chunk"`
	Counters    [5]int64 `json:"counters"`
	Digest      string   `json:"digest"`
}

func (i *Importer) Run(ctx context.Context, source Source, options Options) (model.ImportResult, error) {
	if source == nil || i == nil || i.Transform == nil {
		return model.ImportResult{}, fmt.Errorf("import source and transformer are required")
	}
	if !options.DryRun && i.Destination == nil {
		return model.ImportResult{}, fmt.Errorf("import destination is required")
	}
	if options.ChunkSize == 0 {
		options.ChunkSize = 500
	}
	if options.ChunkSize < 1 || options.ChunkSize > 10_000 {
		return model.ImportResult{}, fmt.Errorf("import chunk size must be between 1 and 10000")
	}
	defer func() { _ = source.Close() }()
	fingerprint, err := source.Fingerprint(ctx)
	if err != nil {
		return model.ImportResult{}, fmt.Errorf("fingerprint import source: %w", err)
	}
	if options.BatchID == "" {
		id, err := model.NewCorrelationID()
		if err != nil {
			return model.ImportResult{}, err
		}
		options.BatchID = "batch-" + id[:12]
	}
	state := checkpoint{Version: 1, BatchID: options.BatchID, SourceKind: source.Kind(), Fingerprint: fingerprint}
	if options.Resume && !options.DryRun {
		encoded, found, err := i.Destination.LoadImportCheckpoint(ctx, options.BatchID)
		if err != nil {
			return model.ImportResult{}, err
		}
		if found {
			if err := json.Unmarshal(encoded, &state); err != nil {
				return model.ImportResult{}, fmt.Errorf("decode import checkpoint: %w", err)
			}
			if err := state.validate(options.BatchID, source.Kind(), fingerprint); err != nil {
				return model.ImportResult{}, err
			}
		}
	}
	result := model.ImportResult{BatchID: options.BatchID, DryRun: options.DryRun}
	result.RowsRead, result.Transformed, result.Inserted, result.Skipped, result.Rejected = state.Counters[0], state.Counters[1], state.Counters[2], state.Counters[3], state.Counters[4]
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		rows, done, err := source.Read(ctx, state.Offset, options.ChunkSize)
		if err != nil {
			return result, fmt.Errorf("read import chunk %d: %w", state.Chunk+1, err)
		}
		events := make([]model.Event, 0, len(rows))
		for _, row := range rows {
			result.RowsRead++
			event, skip, err := safeTransform(ctx, i.Transform, row)
			if skip {
				result.Skipped++
				continue
			}
			if err != nil || event.Validate() != nil {
				result.Rejected++
				continue
			}
			events = append(events, event)
		}
		if options.DryRun {
			result.Transformed += int64(len(events))
		} else {
			inserted, err := i.Destination.WriteImportBatch(ctx, events, options.BatchID)
			if err != nil {
				return result, fmt.Errorf("write import chunk %d: %w", state.Chunk+1, err)
			}
			result.Transformed += inserted
			result.Inserted += inserted
			result.Skipped += int64(len(events)) - inserted
		}
		if len(rows) != 0 {
			state.Offset = rows[len(rows)-1].Offset + 1
		}
		state.Chunk++
		state.Counters = [5]int64{result.RowsRead, result.Transformed, result.Inserted, result.Skipped, result.Rejected}
		if !options.DryRun {
			encoded, err := state.encode()
			if err != nil {
				return result, err
			}
			if err := i.Destination.SaveImportCheckpoint(ctx, options.BatchID, source.Kind(), fingerprint, state.Offset, state.Chunk, encoded, done, state.Counters); err != nil {
				return result, err
			}
		}
		if done {
			break
		}
		if len(rows) == 0 {
			return result, fmt.Errorf("import source returned an empty non-final chunk")
		}
	}
	result.Reconciled = result.RowsRead == result.Transformed+result.Skipped+result.Rejected
	return result, nil
}

func safeTransform(ctx context.Context, transform Transformer, row SourceRow) (event model.Event, skip bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			event = model.Event{}
			skip = false
			err = fmt.Errorf("import transformer panic")
		}
	}()
	return transform(ctx, row)
}

func (c *checkpoint) encode() ([]byte, error) {
	copyState := *c
	copyState.Digest = ""
	canonical, err := json.Marshal(copyState)
	if err != nil {
		return nil, fmt.Errorf("encode import checkpoint: %w", err)
	}
	digest := sha256.Sum256(canonical)
	c.Digest = hex.EncodeToString(digest[:])
	return json.Marshal(c)
}

func (c checkpoint) validate(batchID, sourceKind, fingerprint string) error {
	if c.Version != 1 || c.BatchID != batchID || c.SourceKind != sourceKind || c.Fingerprint != fingerprint || c.Offset < 0 || c.Chunk < 0 {
		return fmt.Errorf("import checkpoint does not match source")
	}
	want := c.Digest
	c.Digest = ""
	canonical, err := json.Marshal(c)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	if want != hex.EncodeToString(digest[:]) {
		return fmt.Errorf("import checkpoint checksum mismatch")
	}
	return nil
}

type SliceSource struct {
	SourceKind string
	ID         string
	Rows       []SourceRow
}

func (s *SliceSource) Kind() string { return s.SourceKind }
func (s *SliceSource) Fingerprint(context.Context) (string, error) {
	digest := sha256.Sum256([]byte(s.SourceKind + "\x00" + s.ID))
	return hex.EncodeToString(digest[:]), nil
}
func (s *SliceSource) Read(_ context.Context, offset int64, limit int) ([]SourceRow, bool, error) {
	start := int(offset)
	if start >= len(s.Rows) {
		return nil, true, nil
	}
	end := start + limit
	if end > len(s.Rows) {
		end = len(s.Rows)
	}
	result := append([]SourceRow(nil), s.Rows[start:end]...)
	for index := range result {
		result[index].Offset = int64(start + index)
	}
	return result, end == len(s.Rows), nil
}
func (s *SliceSource) Close() error { return nil }
