package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
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
	ProviderCredentials(context.Context) ([]model.ProviderCredential, error)
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

func (s *SQLiteStore) ProviderCredentials(ctx context.Context) ([]model.ProviderCredential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrClosed
	}
	rows, err := s.db.QueryContext(ctx, `SELECT credential_id,provider,COALESCE(auth_type,''),
SUM(CASE WHEN succeeded THEN 0 ELSE 1 END),MAX(requested_at_ns)
FROM events WHERE credential_id IS NOT NULL GROUP BY credential_id,provider,auth_type`)
	if err != nil {
		return nil, fmt.Errorf("query provider credentials: %w", err)
	}
	byIdentity := map[string]*model.ProviderCredential{}
	for rows.Next() {
		var credentialID, provider, authType string
		var failed int64
		var observedNS int64
		if err := rows.Scan(&credentialID, &provider, &authType, &failed, &observedNS); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan provider credential: %w", err)
		}
		credential := ensureProviderCredential(byIdentity, provider, credentialID)
		credential.Failed += failed
		observed := time.Unix(0, observedNS).UTC()
		if observed.After(credential.ObservedAt) {
			credential.AuthType = authType
			credential.ObservedAt = observed
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read provider credentials: %w", err)
	}
	_ = rows.Close()
	requestRows, err := s.db.QueryContext(ctx, `SELECT credential_id,provider,COUNT(DISTINCT proxy_request_id),MAX(observed_ns)
FROM (
SELECT credential_id,provider,proxy_request_id,requested_at_ns AS observed_ns FROM events WHERE credential_id IS NOT NULL
UNION ALL
SELECT credential_id,provider,proxy_request_id,bucket_end_ns AS observed_ns FROM request_rollups WHERE credential_id <> ''
) GROUP BY credential_id,provider`)
	if err != nil {
		return nil, fmt.Errorf("query retained provider credential requests: %w", err)
	}
	for requestRows.Next() {
		var credentialID, provider string
		var requests, observedNS int64
		if err := requestRows.Scan(&credentialID, &provider, &requests, &observedNS); err != nil {
			_ = requestRows.Close()
			return nil, fmt.Errorf("scan retained provider credential requests: %w", err)
		}
		credential := ensureProviderCredential(byIdentity, provider, credentialID)
		credential.Requests = requests
		observed := time.Unix(0, observedNS).UTC()
		if observed.After(credential.ObservedAt) {
			credential.ObservedAt = observed
		}
	}
	if err := requestRows.Err(); err != nil {
		_ = requestRows.Close()
		return nil, fmt.Errorf("read retained provider credential requests: %w", err)
	}
	_ = requestRows.Close()
	rollupRows, err := s.db.QueryContext(ctx, `SELECT credential_id,provider,auth_type,
SUM(CASE WHEN succeeded THEN 0 ELSE upstream_attempts END),MAX(last_activity_ns)
FROM rollups WHERE credential_id <> '' GROUP BY credential_id,provider,auth_type`)
	if err != nil {
		return nil, fmt.Errorf("query retained provider credentials: %w", err)
	}
	for rollupRows.Next() {
		var credentialID, provider, authType string
		var failed, observedNS int64
		if err := rollupRows.Scan(&credentialID, &provider, &authType, &failed, &observedNS); err != nil {
			_ = rollupRows.Close()
			return nil, fmt.Errorf("scan retained provider credential: %w", err)
		}
		credential := ensureProviderCredential(byIdentity, provider, credentialID)
		credential.Failed += failed
		observed := time.Unix(0, observedNS).UTC()
		if observed.After(credential.ObservedAt) {
			credential.AuthType = authType
			credential.ObservedAt = observed
		}
	}
	if err := rollupRows.Err(); err != nil {
		_ = rollupRows.Close()
		return nil, fmt.Errorf("read retained provider credentials: %w", err)
	}
	_ = rollupRows.Close()
	errorRows, err := s.db.QueryContext(ctx, `SELECT credential_id,provider,error_class,requested_at_ns
FROM events WHERE credential_id IS NOT NULL AND succeeded=0 AND error_class IS NOT NULL
ORDER BY requested_at_ns`)
	if err != nil {
		return nil, fmt.Errorf("query provider credential errors: %w", err)
	}
	for errorRows.Next() {
		var credentialID, provider, errorClass string
		var errorNS int64
		if err := errorRows.Scan(&credentialID, &provider, &errorClass, &errorNS); err != nil {
			_ = errorRows.Close()
			return nil, fmt.Errorf("scan provider credential error: %w", err)
		}
		if credential := byIdentity[provider+"\x00"+credentialID]; credential != nil {
			observed := time.Unix(0, errorNS).UTC()
			setProviderCredentialError(credential, errorClass, observed)
		}
	}
	if err := errorRows.Err(); err != nil {
		_ = errorRows.Close()
		return nil, fmt.Errorf("read provider credential errors: %w", err)
	}
	_ = errorRows.Close()
	retainedErrorRows, err := s.db.QueryContext(ctx, `SELECT credential_id,provider,error_class,last_activity_ns
FROM rollups WHERE credential_id <> '' AND succeeded=0 AND error_class <> '' ORDER BY last_activity_ns`)
	if err != nil {
		return nil, fmt.Errorf("query retained provider credential errors: %w", err)
	}
	for retainedErrorRows.Next() {
		var credentialID, provider, errorClass string
		var errorNS int64
		if err := retainedErrorRows.Scan(&credentialID, &provider, &errorClass, &errorNS); err != nil {
			_ = retainedErrorRows.Close()
			return nil, fmt.Errorf("scan retained provider credential error: %w", err)
		}
		credential := ensureProviderCredential(byIdentity, provider, credentialID)
		setProviderCredentialError(credential, errorClass, time.Unix(0, errorNS).UTC())
	}
	if err := retainedErrorRows.Err(); err != nil {
		_ = retainedErrorRows.Close()
		return nil, fmt.Errorf("read retained provider credential errors: %w", err)
	}
	_ = retainedErrorRows.Close()
	snapshots, err := providerQuotaSnapshots(ctx, s.db)
	if err != nil {
		return nil, err
	}
	for _, snapshot := range snapshots {
		identity := snapshot.Provider + "\x00" + snapshot.CredentialID
		credential := byIdentity[identity]
		if credential == nil {
			credential = ensureProviderCredential(byIdentity, snapshot.Provider, snapshot.CredentialID)
		}
		status := "available"
		switch {
		case snapshot.Disabled:
			status = "disabled"
		case snapshot.QuotaExceeded:
			status = "quota_exceeded"
		case !snapshot.Available:
			status = "unavailable"
		}
		if providerStatusSeverity(status) > providerStatusSeverity(credential.Status) {
			credential.Status = status
		}
		if credential.Quota == nil {
			credential.Quota = &model.ProviderQuota{}
		}
		if snapshot.NextResetAt != nil && (credential.Quota.ResetsAt == nil || snapshot.NextResetAt.Before(*credential.Quota.ResetsAt)) {
			reset := *snapshot.NextResetAt
			credential.Quota.ResetsAt = &reset
		}
		if snapshot.ObservedAt.After(credential.ObservedAt) {
			credential.ObservedAt = snapshot.ObservedAt
		}
	}
	result := make([]model.ProviderCredential, 0, len(byIdentity))
	for _, credential := range byIdentity {
		result = append(result, *credential)
	}
	slices.SortFunc(result, func(left, right model.ProviderCredential) int {
		if order := cmp.Compare(left.Provider, right.Provider); order != 0 {
			return order
		}
		return cmp.Compare(left.CredentialID, right.CredentialID)
	})
	return result, nil
}

func ensureProviderCredential(credentials map[string]*model.ProviderCredential, provider, credentialID string) *model.ProviderCredential {
	identity := provider + "\x00" + credentialID
	credential := credentials[identity]
	if credential == nil {
		credential = &model.ProviderCredential{CredentialID: credentialID, Provider: provider, Status: "available"}
		credentials[identity] = credential
	}
	return credential
}

func setProviderCredentialError(credential *model.ProviderCredential, errorClass string, observed time.Time) {
	if credential.LastErrorAt != nil && !observed.After(*credential.LastErrorAt) {
		return
	}
	credential.LastErrorClass = &errorClass
	credential.LastErrorAt = &observed
}

func providerStatusSeverity(status string) int {
	switch status {
	case "disabled":
		return 3
	case "quota_exceeded":
		return 2
	case "unavailable":
		return 1
	default:
		return 0
	}
}

func providerQuotaSnapshots(ctx context.Context, database *sql.DB) ([]ProviderQuotaSnapshot, error) {
	rows, err := database.QueryContext(ctx, `SELECT provider,credential_id,model,available,disabled,
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
