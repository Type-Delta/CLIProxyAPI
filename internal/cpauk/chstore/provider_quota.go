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
	requests := map[string]map[string]bool{}
	ensure := func(provider, id string) *model.ProviderCredential {
		key := provider + "\x00" + id
		if byID[key] == nil {
			byID[key] = &model.ProviderCredential{Provider: provider, CredentialID: id, Status: "available"}
		}
		return byID[key]
	}
	addRequest := func(provider, id, request string) {
		key := provider + "\x00" + id
		if requests[key] == nil {
			requests[key] = map[string]bool{}
		}
		requests[key][request] = true
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

	events, err := s.loadEvents(ctx, where, args...)
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		if e.CredentialID == nil || *e.CredentialID == "" {
			continue
		}
		c := ensure(e.Provider, *e.CredentialID)
		addRequest(e.Provider, *e.CredentialID, e.ProxyRequestID)
		if !e.Succeeded {
			c.Failed++
			if e.ErrorClass != nil && (c.LastErrorAt == nil || e.RequestedAt.After(*c.LastErrorAt)) {
				v := *e.ErrorClass
				t := e.RequestedAt
				c.LastErrorClass = &v
				c.LastErrorAt = &t
			}
		}
		if e.RequestedAt.After(c.ObservedAt) {
			c.ObservedAt = e.RequestedAt
			if e.AuthType != nil {
				c.AuthType = *e.AuthType
			} else {
				c.AuthType = ""
			}
		}
	}
	hourly, cutoff := bounds.hourly, bounds.raw
	visible := "is_deleted=0 AND credential_id != '' AND ((grain='hourly' AND bucket_end_ns<=? AND bucket_start_ns>=?) OR (grain='daily' AND bucket_end_ns<=?))"
	visible += s.c4bTablePublicationFilter("analytics_rollups")
	rows, err := s.db.Conn().Query(ctx, "SELECT provider,credential_id,auth_type,succeeded,error_class,upstream_attempts,last_activity_ns FROM "+s.table("analytics_rollups")+" FINAL WHERE "+visible, cutoff, hourly, hourly)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var provider, id, auth, class string
		var succeeded, attempts, ns int64
		if err := rows.Scan(&provider, &id, &auth, &succeeded, &class, &attempts, &ns); err != nil {
			_ = rows.Close()
			return nil, err
		}
		c := ensure(provider, id)
		at := time.Unix(0, ns).UTC()
		if succeeded == 0 {
			c.Failed += attempts
			if class != "" && (c.LastErrorAt == nil || at.After(*c.LastErrorAt)) {
				c.LastErrorClass = &class
				c.LastErrorAt = &at
			}
		}
		if at.After(c.ObservedAt) {
			c.ObservedAt = at
			c.AuthType = auth
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = s.db.Conn().Query(ctx, "SELECT provider,credential_id,proxy_request_id FROM "+s.table("analytics_request_rollups")+" FINAL WHERE "+visible, cutoff, hourly, hourly)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var provider, id, request string
		if err := rows.Scan(&provider, &id, &request); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ensure(provider, id)
		addRequest(provider, id, request)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
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
	for key, c := range byID {
		c.Requests = int64(len(requests[key]))
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
