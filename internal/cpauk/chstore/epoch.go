package chstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	store "github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// StartNewIdentityEpoch archives analytics and replaces it with an empty epoch.
// The caller must stop intake before invoking this operation.
func (s *Store) StartNewIdentityEpoch(ctx context.Context) (store.EpochResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return store.EpochResult{}, ErrClosed
	}
	if err := s.requireLegacyImportDone(ctx); err != nil {
		return store.EpochResult{}, err
	}
	id, err := model.NewCorrelationID()
	if err != nil {
		return store.EpochResult{}, err
	}
	archivePath := s.config.Path + ".identity-epoch-" + id
	if _, err := s.backup(ctx, archivePath); err != nil {
		return store.EpochResult{}, fmt.Errorf("archive analytics identity epoch: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return store.EpochResult{}, err
	}
	epoch, err := model.NewCorrelationID()
	if err != nil {
		return store.EpochResult{}, err
	}
	fingerprint, err := model.IdentityKeyFingerprint(key)
	if err != nil {
		return store.EpochResult{}, err
	}
	empty := analyticsArchive{Format: "cpa-clickhouse-analytics-v1", Tables: make(map[string]archiveTable)}
	for _, name := range analyticsTables {
		schema, err := s.tableSchema(ctx, name)
		if err != nil {
			return store.EpochResult{}, err
		}
		empty.Tables[name] = archiveTable{Schema: schema, Rows: []json.RawMessage{}}
	}
	metadata := empty.Tables["analytics_metadata"]
	for name, value := range map[string]string{"identity_fingerprint": fingerprint, "identity_epoch": epoch, "retention_time_zone": s.config.RetentionTimeZone} {
		row, err := json.Marshal(map[string]any{"key": name, "value": value, "version": s.nextVersion(), "is_deleted": 0})
		if err != nil {
			return store.EpochResult{}, err
		}
		metadata.Rows = append(metadata.Rows, row)
	}
	empty.Tables["analytics_metadata"] = metadata
	data, err := json.Marshal(empty)
	if err != nil {
		return store.EpochResult{}, err
	}
	digest := sha256.Sum256(data)
	keyDigest := sha256.Sum256(key)
	temporary := filepath.Join(filepath.Dir(s.config.Path), ".identity-epoch-"+id)
	manifest := BackupManifest{SchemaVersion: s.currentSchema, IdentityEpoch: epoch, DatabaseFile: filepath.Base(temporary), DatabaseSHA256: hex.EncodeToString(digest[:]), IdentityKeyFile: filepath.Base(temporary) + ".identity.key", IdentityKeySHA256: hex.EncodeToString(keyDigest[:]), IdentityKeyFingerprint: fingerprint, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return store.EpochResult{}, err
	}
	files := []struct {
		path string
		data []byte
	}{{temporary, data}, {temporary + ".identity.key", key}, {temporary + ".manifest.json", encoded}}
	for _, file := range files {
		defer func(path string) { _ = os.Remove(path) }(file.path)
		if err := os.WriteFile(file.path, file.data, 0600); err != nil {
			return store.EpochResult{}, err
		}
	}
	if err := s.restore(ctx, temporary, temporary+".manifest.json"); err != nil {
		return store.EpochResult{}, fmt.Errorf("create identity epoch: %w", err)
	}
	return store.EpochResult{IdentityEpoch: epoch, ArchivedDB: archivePath, ArchivedKey: archivePath + ".identity.key"}, nil
}
