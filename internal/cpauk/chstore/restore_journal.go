package chstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

const restoreJob = "analytics-restore:active"

var errRestoreIncomplete = errors.New("analytics restore staging is incomplete")

type restoreTable struct {
	Name    string `json:"name"`
	Stage   string `json:"stage"`
	OldUUID string `json:"old_uuid"`
	NewUUID string `json:"new_uuid"`
}
type restoreJournal struct {
	Tables      []restoreTable `json:"tables"`
	Exchanged   int            `json:"exchanged"`
	KeyFile     string         `json:"key_file"`
	Fingerprint string         `json:"fingerprint"`
}
type restoreTestHookKey struct{}

func (s *Store) tableUUID(ctx context.Context, table string) (string, error) {
	var uuid string
	err := s.db.Conn().QueryRow(ctx, "SELECT toString(uuid) FROM system.tables WHERE database=? AND name=?", s.db.Database(), table).Scan(&uuid)
	return uuid, err
}

func (s *Store) recoverRestore(ctx context.Context) error {
	job, found, err := s.db.Job(ctx, restoreJob)
	if err != nil {
		return err
	}
	if !found || job.State != "running" {
		return nil
	}
	var journal restoreJournal
	if err := json.Unmarshal([]byte(job.Detail), &journal); err != nil {
		return fmt.Errorf("invalid analytics restore journal: %w", err)
	}
	if len(journal.Tables) != len(analyticsTables) || !strings.HasPrefix(filepath.Base(journal.KeyFile), ".c4-restore-key-") || filepath.Dir(journal.KeyFile) != filepath.Dir(s.config.IdentityKeyPath) || !model.IsFullKeyID(journal.Fingerprint) {
		return fmt.Errorf("invalid analytics restore journal inventory")
	}
	for i, table := range journal.Tables {
		if table.Name != analyticsTables[i] || !strings.HasPrefix(table.Stage, "c4_restore_") || table.OldUUID == "" || table.NewUUID == "" || table.OldUUID == table.NewUUID {
			return fmt.Errorf("invalid analytics restore table journal")
		}
	}
	err = s.finishRestore(ctx, &journal, nil)
	if errors.Is(err, errRestoreIncomplete) {
		return nil
	}
	return err
}

func (s *Store) finishRestore(ctx context.Context, journal *restoreJournal, hook func(int) error) error {
	save := func(state string) error {
		detail, err := json.Marshal(journal)
		if err != nil {
			return err
		}
		return s.db.SetJob(ctx, restoreJob, state, string(detail))
	}
	// Never start publication when staging is incomplete. If publication has
	// already begun and a stage is missing, fail closed rather than expose a
	// mixture of old and restored tables.
	missing, allOld := false, true
	for _, table := range journal.Tables {
		live, err := s.tableUUID(ctx, table.Name)
		if err != nil {
			return fmt.Errorf("inspect restore live table %s: %w", table.Name, err)
		}
		if live != table.OldUUID {
			allOld = false
		}
		_, err = s.tableUUID(ctx, table.Stage)
		if errors.Is(err, sql.ErrNoRows) {
			missing = true
		} else if err != nil {
			return err
		}
	}
	if missing {
		if !allOld {
			return fmt.Errorf("restore staging is incomplete after publication; recovery requires operator intervention")
		}
		for _, table := range journal.Tables {
			if err := s.db.Conn().Exec(ctx, "DROP TABLE IF EXISTS "+s.table(table.Stage)+" SYNC"); err != nil {
				return err
			}
		}
		if err := os.Remove(journal.KeyFile); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := save("failed"); err != nil {
			return err
		}
		return errRestoreIncomplete
	}
	if hook != nil {
		if err := hook(0); err != nil {
			return err
		}
	}
	// UUIDs disambiguate a crash between EXCHANGE and the following journal update.
	for i, table := range journal.Tables {
		live, err := s.tableUUID(ctx, table.Name)
		if err != nil {
			return fmt.Errorf("inspect restore live table %s: %w", table.Name, err)
		}
		stage, err := s.tableUUID(ctx, table.Stage)
		if err != nil {
			return fmt.Errorf("inspect restore staging table %s; recovery requires operator intervention: %w", table.Stage, err)
		}
		switch {
		case live == table.OldUUID && stage == table.NewUUID:
			if err := s.db.Conn().Exec(ctx, "EXCHANGE TABLES "+s.table(table.Name)+" AND "+s.table(table.Stage)); err != nil {
				return fmt.Errorf("publish restored %s: %w", table.Name, err)
			}
		case live == table.NewUUID && stage == table.OldUUID:
		default:
			return fmt.Errorf("restore table identities changed for %s", table.Name)
		}
		if hook != nil {
			if err := hook(i + 1); err != nil {
				return err
			}
		}
		journal.Exchanged = i + 1
		if err := save("running"); err != nil {
			return err
		}
	}
	key, err := os.ReadFile(journal.KeyFile)
	if os.IsNotExist(err) {
		key, err = os.ReadFile(s.config.IdentityKeyPath)
	}
	if err != nil {
		return fmt.Errorf("read restore identity key: %w", err)
	}
	fingerprint, err := model.IdentityKeyFingerprint(key)
	if err != nil || fingerprint != journal.Fingerprint {
		return fmt.Errorf("restore identity key mismatch")
	}
	if _, err := os.Stat(journal.KeyFile); err == nil {
		if err := os.Rename(journal.KeyFile, s.config.IdentityKeyPath); err != nil {
			return fmt.Errorf("publish restore identity key: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect restore identity key: %w", err)
	}
	if hook != nil {
		if err := hook(len(journal.Tables) + 1); err != nil {
			return err
		}
	}
	if dir, err := os.Open(filepath.Dir(s.config.IdentityKeyPath)); err == nil {
		err = dir.Sync()
		_ = dir.Close()
		if err != nil {
			return err
		}
	} else {
		return err
	}
	if err := save("done"); err != nil {
		return err
	}
	for _, table := range journal.Tables {
		if err := s.db.Conn().Exec(ctx, "DROP TABLE IF EXISTS "+s.table(table.Stage)+" SYNC"); err != nil {
			return fmt.Errorf("clean restore staging: %w", err)
		}
	}
	return nil
}
