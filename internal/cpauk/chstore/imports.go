package chstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type importCheckpoint struct {
	SourceKind  string    `json:"source_kind"`
	Fingerprint string    `json:"source_fingerprint"`
	Offset      int64     `json:"offset"`
	Chunk       int       `json:"chunk"`
	Checkpoint  []byte    `json:"checkpoint"`
	Completed   bool      `json:"completed"`
	Counters    [5]int64  `json:"counters"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (s *Store) LoadImportCheckpoint(ctx context.Context, batchID string) ([]byte, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, false, ErrClosed
	}
	values, err := s.loadControl(ctx, "import_checkpoints")
	if err != nil {
		return nil, false, fmt.Errorf("load import checkpoints: %w", err)
	}
	raw, found := values[batchID]
	if !found {
		return nil, false, nil
	}
	var checkpoint importCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return nil, false, fmt.Errorf("decode import checkpoint: %w", err)
	}
	return checkpoint.Checkpoint, true, nil
}

func (s *Store) SaveImportCheckpoint(ctx context.Context, batchID, sourceKind, fingerprint string, offset int64, chunk int, checkpoint []byte, completed bool, counters [5]int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	values, err := s.loadControl(ctx, "import_checkpoints")
	if err != nil {
		return fmt.Errorf("load import checkpoints: %w", err)
	}
	value, err := json.Marshal(importCheckpoint{sourceKind, fingerprint, offset, chunk, checkpoint, completed, counters, time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("encode import checkpoint: %w", err)
	}
	values[batchID] = value
	if err := s.replaceControl(ctx, "import_checkpoints", values); err != nil {
		return fmt.Errorf("save import checkpoint: %w", err)
	}
	return nil
}
