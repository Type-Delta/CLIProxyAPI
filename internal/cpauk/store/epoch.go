package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

type EpochResult struct {
	Store         *SQLiteStore
	IdentityEpoch string
	ArchivedDB    string
	ArchivedKey   string
}

// StartNewIdentityEpoch detaches this store from its current files, archives
// them through the standalone recovery path, and adopts the new empty store.
// Callers must stop intake and drain in-flight writes before invoking it.
func (s *SQLiteStore) StartNewIdentityEpoch(ctx context.Context) (EpochResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return EpochResult{}, ErrClosed
	}
	config := s.config
	oldDatabase := s.db
	s.db = nil
	if err := oldDatabase.Close(); err != nil {
		s.db = oldDatabase
		return EpochResult{}, fmt.Errorf("close analytics database before identity recovery: %w", err)
	}
	result, err := StartNewIdentityEpoch(ctx, config)
	if err != nil {
		candidate, reopenErr := Open(ctx, config)
		if reopenErr == nil {
			s.adopt(candidate)
		}
		return EpochResult{}, err
	}
	s.adopt(result.Store)
	result.Store = s
	return result, nil
}

func (s *SQLiteStore) adopt(candidate *SQLiteStore) {
	candidate.mu.Lock()
	defer candidate.mu.Unlock()
	s.db = candidate.db
	s.config = candidate.config
	s.identityKey = candidate.identityKey
	s.identityEpoch = candidate.identityEpoch
	s.currentSchema = candidate.currentSchema
	s.retentionCutoff = candidate.retentionCutoff
	candidate.db = nil
}

// StartNewIdentityEpoch is the explicit recovery path for a corrupt database
// or a lost identity key. The caller must detach intake and close every handle
// to config.Path before calling it. It archives source files and creates an
// empty database; it never writes into the old database.
func StartNewIdentityEpoch(ctx context.Context, config Config) (EpochResult, error) {
	if err := config.normalize(); err != nil {
		return EpochResult{}, err
	}
	if _, err := os.Stat(config.Path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return EpochResult{}, fmt.Errorf("analytics database does not exist")
		}
		return EpochResult{}, fmt.Errorf("inspect analytics database: %w", err)
	}
	recoveryID, err := newRestoreID()
	if err != nil {
		return EpochResult{}, err
	}
	archiveDatabase := config.Path + ".identity-epoch-" + recoveryID
	archiveIdentity := config.IdentityKeyPath + ".identity-epoch-" + recoveryID
	moved := make([][2]string, 0, 4)
	move := func(source, destination string, required bool) error {
		if err := os.Rename(source, destination); err != nil {
			if !required && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		moved = append(moved, [2]string{source, destination})
		return nil
	}
	rollback := func() {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(config.Path + suffix)
		}
		_ = os.Remove(config.IdentityKeyPath)
		for index := len(moved) - 1; index >= 0; index-- {
			_ = os.Rename(moved[index][1], moved[index][0])
		}
	}
	if err := move(config.Path, archiveDatabase, true); err != nil {
		return EpochResult{}, fmt.Errorf("archive analytics database: %w", err)
	}
	if err := move(config.Path+"-wal", archiveDatabase+"-wal", false); err != nil {
		rollback()
		return EpochResult{}, fmt.Errorf("archive analytics WAL: %w", err)
	}
	if err := move(config.Path+"-shm", archiveDatabase+"-shm", false); err != nil {
		rollback()
		return EpochResult{}, fmt.Errorf("archive analytics shared-memory file: %w", err)
	}
	if err := move(config.IdentityKeyPath, archiveIdentity, false); err != nil {
		rollback()
		return EpochResult{}, fmt.Errorf("archive analytics identity key: %w", err)
	}
	database, err := Open(ctx, config)
	if err != nil {
		rollback()
		return EpochResult{}, fmt.Errorf("create new analytics identity epoch: %w", err)
	}
	return EpochResult{Store: database, IdentityEpoch: database.IdentityEpoch(), ArchivedDB: archiveDatabase, ArchivedKey: archiveIdentity}, nil
}
