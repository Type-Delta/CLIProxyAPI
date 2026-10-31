package chstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	log "github.com/sirupsen/logrus"
)

func (s *Store) metadata(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.Conn().QueryRow(ctx, "SELECT value FROM "+s.table("analytics_metadata")+" FINAL WHERE key=? AND is_deleted=0", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}
func (s *Store) setMetadata(ctx context.Context, key, value string) error {
	return s.setMetadataBatch(ctx, map[string]string{key: value})
}
func (s *Store) setMetadataBatch(ctx context.Context, values map[string]string) error {
	defer s.clearHistory()
	batch, err := s.db.Conn().PrepareBatch(synchronousWriteContext(ctx), "INSERT INTO "+s.table("analytics_metadata")+" (key,value,version,is_deleted)")
	if err != nil {
		return err
	}
	defer func() { _ = batch.Abort() }()
	version := s.nextVersion()
	for key, value := range values {
		if err := batch.Append(key, value, version, uint8(0)); err != nil {
			return err
		}
	}
	return batch.Send()
}
func (s *Store) loadControl(ctx context.Context, namespace string) (map[string]json.RawMessage, error) {
	values := make(map[string]json.RawMessage)
	var generation uint64
	err := s.db.Conn().QueryRow(ctx, "SELECT generation FROM "+s.table("analytics_control_heads")+" FINAL WHERE namespace=?", namespace).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return values, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Conn().Query(ctx, "SELECT key,value FROM "+s.table("analytics_control")+" FINAL WHERE namespace=? AND generation=? AND is_deleted=0", namespace, generation)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		values[key] = json.RawMessage(value)
	}
	return values, rows.Err()
}
func (s *Store) replaceControl(ctx context.Context, namespace string, values map[string]json.RawMessage) error {
	return s.replaceControlMode(ctx, namespace, values, true)
}

func (s *Store) replaceControlMode(ctx context.Context, namespace string, values map[string]json.RawMessage, enforceBudget bool) error {
	if namespace == "pricing" {
		defer s.clearHistory()
	}
	for key, value := range values {
		if !json.Valid(value) {
			return fmt.Errorf("invalid analytics control JSON for %s/%s", namespace, key)
		}
	}
	var estimated int64 = 128
	for key, value := range values {
		estimated += int64(len(namespace) + len(key) + len(value) + 64)
	}
	additional := estimated
	if enforceBudget && namespace == "key_lifecycle" {
		// Startup must be able to refresh existing lifecycle state even while
		// over quota. Only newly retained identities grow this collection.
		existing, err := s.loadControl(ctx, namespace)
		if err != nil {
			return err
		}
		additional = 0
		for key, value := range values {
			if _, present := existing[key]; !present {
				additional += int64(len(namespace) + len(key) + len(value) + 64)
			}
		}
		if additional > 0 {
			additional += 128
		}
	}
	// Recovery journals stay writable while maintenance frees space.
	if enforceBudget && namespace != "retention" && namespace != "late_retention" && additional > 0 {
		if err := s.checkQuota(ctx, additional); err != nil {
			return err
		}
	}
	generation := s.nextVersion()
	if len(values) > 0 {
		batch, err := s.db.Conn().PrepareBatch(synchronousWriteContext(ctx), "INSERT INTO "+s.table("analytics_control")+" (namespace,generation,key,value,version,is_deleted)")
		if err != nil {
			return err
		}
		defer func() { _ = batch.Abort() }()
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := batch.Append(namespace, generation, key, string(values[key]), generation, uint8(0)); err != nil {
				return err
			}
		}
		if err := batch.Send(); err != nil {
			return fmt.Errorf("stage analytics control: %w", err)
		}
	}
	if err := s.db.Conn().Exec(synchronousWriteContext(ctx), "INSERT INTO "+s.table("analytics_control_heads")+" (namespace,generation,version) VALUES (?,?,?)", namespace, generation, s.nextVersion()); err != nil {
		return err
	}
	// Publishing the head commits this snapshot. Reclamation cannot undo it.
	errReclaim := c4bStep(ctx, "control:published")
	s.reserveBudget(additional)
	// Every reader holds mu.RLock, so no reader can still use the old head.
	if errReclaim == nil {
		errReclaim = s.c4bDelete(ctx, "analytics_control", "namespace=? AND generation!=?", namespace, generation)
	}
	if errReclaim != nil {
		log.WithError(errReclaim).WithField("namespace", namespace).Warn("reclaim superseded analytics control generations")
	}
	return nil
}
