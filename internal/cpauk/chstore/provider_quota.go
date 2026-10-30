package chstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	store "github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func (s *Store) ReplaceProviderQuotaSnapshots(ctx context.Context, snapshots []ProviderQuotaSnapshot) error {
	if len(snapshots) > store.MaxProviderQuotaSnapshots {
		return fmt.Errorf("provider quota snapshot exceeds %d rows", store.MaxProviderQuotaSnapshots)
	}
	values := make(map[string]json.RawMessage, len(snapshots))
	for i, item := range snapshots {
		if err := validateProviderQuotaSnapshot(item); err != nil {
			return fmt.Errorf("provider quota snapshot %d: %w", i, err)
		}
		keyBytes, _ := json.Marshal([]string{item.Provider, item.CredentialID, item.Model})
		key := string(keyBytes)
		if _, ok := values[key]; ok {
			return fmt.Errorf("provider quota snapshot %d is duplicated", i)
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return err
		}
		values[key] = raw
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	return s.replaceControl(ctx, "provider_quota", values)
}
func (s *Store) providerQuotaSnapshots(ctx context.Context) ([]ProviderQuotaSnapshot, error) {
	values, err := s.loadControl(ctx, "provider_quota")
	if err != nil {
		return nil, err
	}
	result := make([]ProviderQuotaSnapshot, 0, len(values))
	for _, raw := range values {
		var item ProviderQuotaSnapshot
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		item.ObservedAt = item.ObservedAt.UTC()
		if item.NextResetAt != nil {
			t := item.NextResetAt.UTC()
			item.NextResetAt = &t
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.CredentialID != b.CredentialID {
			return a.CredentialID < b.CredentialID
		}
		return a.Model < b.Model
	})
	return result, nil
}
func (s *Store) ProviderQuotaSnapshots(ctx context.Context) ([]ProviderQuotaSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrClosed
	}
	return s.providerQuotaSnapshots(ctx)
}
func validateProviderQuotaSnapshot(item ProviderQuotaSnapshot) error {
	if item.Provider == "" || len(item.Provider) > model.MaxStoredStringBytes || len(item.Model) > model.MaxStoredStringBytes {
		return fmt.Errorf("provider and model must be bounded")
	}
	if !model.IsFullKeyID(item.CredentialID) {
		return fmt.Errorf("credential ID must be a full sanitized hash")
	}
	if item.ObservedAt.IsZero() || item.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("observed timestamp must be UTC")
	}
	if item.NextResetAt != nil && (item.NextResetAt.IsZero() || item.NextResetAt.Location() != time.UTC) {
		return fmt.Errorf("next reset timestamp must be UTC")
	}
	return nil
}

func (s *Store) ProviderCredentials(ctx context.Context) ([]model.ProviderCredential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrClosed
	}
	byID := map[string]*model.ProviderCredential{}
	ensure := func(provider, id string) *model.ProviderCredential {
		key := provider + "\x00" + id
		if byID[key] == nil {
			byID[key] = &model.ProviderCredential{Provider: provider, CredentialID: id, Status: "available"}
		}
		return byID[key]
	}
	bounds, err := s.readCutoffs(ctx)
	if err != nil {
		return nil, err
	}
	where := "1=1"
	args := []any{}
	if bounds.raw != 0 {
		where = "requested_at_ns >= ?"
		args = append(args, bounds.raw)
	}

	// These reports consume attempt fields only. Response patches do not change
	// succeeded, auth_type, error_class, or requested_at_ns.
	raw := "SELECT provider,credential_id,proxy_request_id FROM " + s.table("analytics_events") + " FINAL WHERE is_deleted=0 AND credential_id != '' AND (" + where + ")"
	hourly, cutoff := bounds.hourly, bounds.raw
	visible := "is_deleted=0 AND credential_id != '' AND ((grain='hourly' AND bucket_end_ns<=? AND bucket_start_ns>=?) OR (grain='daily' AND bucket_end_ns<=?))"
	rollupWhere := visible + s.c4bTablePublicationFilter("analytics_rollups")
	retained := "SELECT provider,credential_id,proxy_request_id FROM " + s.table("analytics_request_rollups") + " FINAL WHERE " + visible + s.c4bTablePublicationFilter("analytics_rollups")
	rows, err := s.db.Conn().Query(ctx, "SELECT provider,credential_id,uniqExact(proxy_request_id) FROM ("+raw+" UNION ALL "+retained+") GROUP BY provider,credential_id", append(args, cutoff, hourly, hourly)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var provider, id string
		var requests uint64
		if err := rows.Scan(&provider, &id, &requests); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ensure(provider, id).Requests = int64(requests)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}

	// Earliest storage key wins timestamp ties, as in the former ordered scan.
	// Raw observations are merged first, so they win ties against retained rows.
	for _, source := range []struct {
		sql  string
		args []any
	}{
		{"SELECT provider,credential_id,toInt64(countIf(succeeded=0)),max(requested_at_ns),argMin(coalesce(auth_type,''),tuple(-requested_at_ns,attempt_id)),countIf(succeeded=0 AND error_class IS NOT NULL),maxIf(requested_at_ns,succeeded=0 AND error_class IS NOT NULL),argMinIf(coalesce(error_class,''),tuple(-requested_at_ns,attempt_id),succeeded=0 AND error_class IS NOT NULL) FROM " + s.table("analytics_events") + " FINAL WHERE is_deleted=0 AND credential_id != '' AND (" + where + ") GROUP BY provider,credential_id", args},
		{"SELECT provider,credential_id,sumIf(upstream_attempts,succeeded=0),max(last_activity_ns),argMin(auth_type," + providerQuotaRetainedOrder + "),countIf(succeeded=0 AND error_class!=''),maxIf(last_activity_ns,succeeded=0 AND error_class!=''),argMinIf(error_class," + providerQuotaRetainedOrder + ",succeeded=0 AND error_class!='') FROM " + s.table("analytics_rollups") + " FINAL WHERE " + rollupWhere + " GROUP BY provider,credential_id", []any{cutoff, hourly, hourly}},
	} {
		rows, err := s.db.Conn().Query(ctx, source.sql, source.args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var provider, id, auth, class string
			var failed, observed, errorAt int64
			var errors uint64
			if err := rows.Scan(&provider, &id, &failed, &observed, &auth, &errors, &errorAt, &class); err != nil {
				_ = rows.Close()
				return nil, err
			}
			c := ensure(provider, id)
			c.Failed += failed
			at := time.Unix(0, observed).UTC()
			if at.After(c.ObservedAt) {
				c.ObservedAt = at
				c.AuthType = auth
			}
			at = time.Unix(0, errorAt).UTC()
			if errors > 0 && (c.LastErrorAt == nil || at.After(*c.LastErrorAt)) {
				c.LastErrorAt = &at
				c.LastErrorClass = &class
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}

	snapshots, err := s.providerQuotaSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range snapshots {
		c := ensure(item.Provider, item.CredentialID)
		status := "available"
		if item.Disabled {
			status = "disabled"
		} else if item.QuotaExceeded {
			status = "quota_exceeded"
		} else if !item.Available {
			status = "unavailable"
		}
		if providerStatusSeverity(status) > providerStatusSeverity(c.Status) {
			c.Status = status
		}
		if c.Quota == nil {
			c.Quota = &model.ProviderQuota{}
		}
		if item.NextResetAt != nil && (c.Quota.ResetsAt == nil || item.NextResetAt.Before(*c.Quota.ResetsAt)) {
			v := *item.NextResetAt
			c.Quota.ResetsAt = &v
		}
		if item.ObservedAt.After(c.ObservedAt) {
			c.ObservedAt = item.ObservedAt
		}
	}
	result := make([]model.ProviderCredential, 0, len(byID))
	for _, c := range byID {
		result = append(result, *c)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Provider != result[j].Provider {
			return result[i].Provider < result[j].Provider
		}
		return result[i].CredentialID < result[j].CredentialID
	})
	return result, nil
}
func providerStatusSeverity(status string) int {
	switch status {
	case "disabled":
		return 3
	case "quota_exceeded":
		return 2
	case "unavailable":
		return 1
	}
	return 0
}

// The rollup primary key supplies the first observation when timestamps tie.
const providerQuotaRetainedOrder = "tuple(-last_activity_ns,grain,bucket_start_ns,key_id,provider,model,credential_id,endpoint_class,auth_type,service_tier,succeeded,error_class,status_code,token_quality,latency_bucket,cache_class,import_batch_id,generation)"
