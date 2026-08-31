package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

const MaxProviderQuotaSnapshots = 10_000

// ProviderQuotaSnapshot intentionally contains only sanitized identifiers and
// bounded state. CredentialID must be the CPAUK identity-key HMAC, never an
// auth ID, file name, token, or other source credential value.
type ProviderQuotaSnapshot struct {
	Provider      string
	CredentialID  string
	Model         string
	Available     bool
	Disabled      bool
	QuotaExceeded bool
	NextResetAt   *time.Time
	ObservedAt    time.Time
}

type ProviderQuotaStore interface {
	ReplaceProviderQuotaSnapshots(context.Context, []ProviderQuotaSnapshot) error
	ProviderQuotaSnapshots(context.Context) ([]ProviderQuotaSnapshot, error)
}

func (s *SQLiteStore) ReplaceProviderQuotaSnapshots(ctx context.Context, snapshots []ProviderQuotaSnapshot) error {
	if len(snapshots) > MaxProviderQuotaSnapshots {
		return fmt.Errorf("provider quota snapshot exceeds %d rows", MaxProviderQuotaSnapshots)
	}
	seen := make(map[string]struct{}, len(snapshots))
	for index := range snapshots {
		if err := validateProviderQuotaSnapshot(snapshots[index]); err != nil {
			return fmt.Errorf("provider quota snapshot %d: %w", index, err)
		}
		key := snapshots[index].Provider + "\x00" + snapshots[index].CredentialID + "\x00" + snapshots[index].Model
		if _, exists := seen[key]; exists {
			return fmt.Errorf("provider quota snapshot %d is duplicated", index)
		}
		seen[key] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin provider quota snapshot update: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM provider_quota_snapshots"); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("clear provider quota snapshots: %w", err)
	}
	for index := range snapshots {
		var nextReset any
		if snapshots[index].NextResetAt != nil {
			nextReset = snapshots[index].NextResetAt.UnixNano()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO provider_quota_snapshots
(provider,credential_id,model,available,disabled,quota_exceeded,next_reset_at_ns,observed_at_ns)
VALUES (?,?,?,?,?,?,?,?)`, snapshots[index].Provider, snapshots[index].CredentialID, snapshots[index].Model,
			snapshots[index].Available, snapshots[index].Disabled, snapshots[index].QuotaExceeded,
			nextReset, snapshots[index].ObservedAt.UnixNano()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert provider quota snapshot %d: %w", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit provider quota snapshot: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ProviderQuotaSnapshots(ctx context.Context) ([]ProviderQuotaSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrClosed
	}
	rows, err := s.db.QueryContext(ctx, `SELECT provider,credential_id,model,available,disabled,
quota_exceeded,next_reset_at_ns,observed_at_ns FROM provider_quota_snapshots
ORDER BY provider,credential_id,model`)
	if err != nil {
		return nil, fmt.Errorf("query provider quota snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]ProviderQuotaSnapshot, 0)
	for rows.Next() {
		var snapshot ProviderQuotaSnapshot
		var nextReset sql.NullInt64
		var observedAt int64
		if err := rows.Scan(&snapshot.Provider, &snapshot.CredentialID, &snapshot.Model, &snapshot.Available,
			&snapshot.Disabled, &snapshot.QuotaExceeded, &nextReset, &observedAt); err != nil {
			return nil, fmt.Errorf("scan provider quota snapshot: %w", err)
		}
		if nextReset.Valid {
			value := time.Unix(0, nextReset.Int64).UTC()
			snapshot.NextResetAt = &value
		}
		snapshot.ObservedAt = time.Unix(0, observedAt).UTC()
		result = append(result, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read provider quota snapshots: %w", err)
	}
	return result, nil
}

func validateProviderQuotaSnapshot(snapshot ProviderQuotaSnapshot) error {
	if snapshot.Provider == "" || len(snapshot.Provider) > model.MaxStoredStringBytes ||
		snapshot.Model != "" && len(snapshot.Model) > model.MaxStoredStringBytes {
		return fmt.Errorf("provider and model must be bounded")
	}
	if !model.IsFullKeyID(snapshot.CredentialID) {
		return fmt.Errorf("credential ID must be a full sanitized hash")
	}
	if snapshot.ObservedAt.IsZero() || snapshot.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("observed timestamp must be UTC")
	}
	if snapshot.NextResetAt != nil && (snapshot.NextResetAt.IsZero() || snapshot.NextResetAt.Location() != time.UTC) {
		return fmt.Errorf("next reset timestamp must be UTC")
	}
	return nil
}
