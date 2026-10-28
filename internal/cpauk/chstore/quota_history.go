package chstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// quotaHistoryRow is the sanitized legacy sample, including its original time.
type quotaHistoryRow = QuotaHistorySample

// appendQuotaHistoryRows preserves observed times and is retry-safe for identical
// imported samples. The caller holds s.mu while mutating the store.
func (s *Store) appendQuotaHistoryRows(ctx context.Context, rows []quotaHistoryRow) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(encoded)
		ids[i] = fmt.Sprintf("import:%x", sum)
	}
	existing := map[string]bool{}
	for start := 0; start < len(ids); start += 1000 {
		end := min(start+1000, len(ids))
		found, err := s.db.Conn().Query(ctx, "SELECT row_id FROM "+s.table("analytics_quota_history")+" WHERE row_id IN (?)", ids[start:end])
		if err != nil {
			return err
		}
		for found.Next() {
			var id string
			if err := found.Scan(&id); err != nil {
				_ = found.Close()
				return err
			}
			existing[id] = true
		}
		err = found.Err()
		_ = found.Close()
		if err != nil {
			return err
		}
	}
	return s.appendQuotaHistory(ctx, rows, ids, existing)
}
func (s *Store) appendQuotaHistory(ctx context.Context, rows []QuotaHistorySample, ids []string, existing map[string]bool) error {
	if len(rows) == 0 {
		return nil
	}
	var estimated int64
	for i, row := range rows {
		if !existing[ids[i]] {
			encoded, err := json.Marshal(row)
			if err != nil {
				return err
			}
			estimated += int64(len(encoded) + len(ids[i]) + 64)
		}
	}
	if estimated == 0 {
		return nil
	}
	if err := s.checkQuota(ctx, estimated); err != nil {
		return err
	}
	batch, err := s.db.Conn().PrepareBatch(ctx, "INSERT INTO "+s.table("analytics_quota_history")+" (provider,credential_id,window_id,observed_at_ns,remaining_percent,reset_at_ns,period_hours,plan_id,capacity,source,row_id)")
	if err != nil {
		return err
	}
	defer func() { _ = batch.Abort() }()
	count := 0
	for i, item := range rows {
		if existing[ids[i]] {
			continue
		}
		existing[ids[i]] = true
		var reset any
		if item.ResetAt != nil {
			reset = item.ResetAt.UnixNano()
		}
		var period, capacity any
		if item.PeriodHours != nil {
			period = *item.PeriodHours
		}
		if item.Capacity != nil {
			capacity = *item.Capacity
		}
		if err := batch.Append(item.Provider, item.CredentialID, item.WindowID, item.ObservedAt.UnixNano(), item.RemainingPercent, reset, period, item.PlanID, capacity, item.Source, ids[i]); err != nil {
			return err
		}
		count++
	}
	if count == 0 {
		return nil
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("append quota history: %w", err)
	}
	s.reserveBudget(estimated)
	return nil
}
func (s *Store) RecordQuotaHistory(ctx context.Context, samples []QuotaHistorySample, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	for _, item := range samples {
		if !model.IsFullKeyID(item.CredentialID) || item.Provider == "" || len(item.Provider) > 64 || item.WindowID == "" || len(item.WindowID) > 64 || len(item.PlanID) > 64 || (item.Source != "cpa_auto" && item.Source != "cpamc") || math.IsNaN(item.RemainingPercent) || math.IsInf(item.RemainingPercent, 0) || item.RemainingPercent < 0 || item.RemainingPercent > 100 || !validQuotaHistoryFloat(item.Capacity, 1e12) || !validQuotaHistoryFloat(item.PeriodHours, 24*366) {
			return fmt.Errorf("invalid quota history sample")
		}
	}
	cutoff := now.Add(-90 * 24 * time.Hour).UnixNano()
	var expired uint64
	if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table("analytics_quota_history")+" WHERE observed_at_ns < ?", cutoff).Scan(&expired); err != nil {
		return err
	}
	if expired > 0 {
		if err := s.db.Conn().Exec(ctx, "ALTER TABLE "+s.table("analytics_quota_history")+" DELETE WHERE observed_at_ns < ? SETTINGS mutations_sync=2", cutoff); err != nil {
			return fmt.Errorf("prune quota history: %w", err)
		}
	}
	providers := map[string]bool{}
	for _, item := range samples {
		providers[item.Provider] = true
	}
	providerNames := make([]string, 0, len(providers))
	for provider := range providers {
		providerNames = append(providerNames, provider)
	}
	sort.Strings(providerNames)
	last := map[[3]string]int64{}
	if len(providerNames) > 0 {
		found, err := s.db.Conn().Query(ctx, "SELECT provider,credential_id,window_id,max(observed_at_ns) FROM "+s.table("analytics_quota_history")+" WHERE provider IN (?) AND observed_at_ns >= ? GROUP BY provider,credential_id,window_id", providerNames, max(cutoff, now.Add(-5*time.Minute).UnixNano()))
		if err != nil {
			return err
		}
		for found.Next() {
			var p, c, w string
			var ns int64
			if err := found.Scan(&p, &c, &w, &ns); err != nil {
				_ = found.Close()
				return err
			}
			last[[3]string{p, c, w}] = ns
		}
		err = found.Err()
		_ = found.Close()
		if err != nil {
			return err
		}
	}
	pending := make([]QuotaHistorySample, 0, len(samples))
	ids := make([]string, 0, len(samples))
	for _, item := range samples {
		key := [3]string{item.Provider, item.CredentialID, item.WindowID}
		if ns, ok := last[key]; ok && now.UnixNano()-ns < int64(5*time.Minute) {
			continue
		}
		item.ObservedAt = now.UTC()
		last[key] = now.UnixNano()
		pending = append(pending, item)
		ids = append(ids, fmt.Sprintf("live:%020d", s.nextVersion()))
	}
	if err := s.appendQuotaHistory(ctx, pending, ids, map[string]bool{}); err != nil {
		return err
	}
	for _, provider := range providerNames {
		var count uint64
		if err := s.db.Conn().QueryRow(ctx, "SELECT count() FROM "+s.table("analytics_quota_history")+" WHERE provider=?", provider).Scan(&count); err != nil {
			return err
		}
		if count <= 100000 {
			continue
		}
		var ns int64
		var id string
		if err := s.db.Conn().QueryRow(ctx, "SELECT observed_at_ns,row_id FROM "+s.table("analytics_quota_history")+" WHERE provider=? ORDER BY observed_at_ns DESC,row_id ASC LIMIT 1 OFFSET 99999", provider).Scan(&ns, &id); err != nil {
			return err
		}
		if err := s.db.Conn().Exec(ctx, "ALTER TABLE "+s.table("analytics_quota_history")+" DELETE WHERE provider=? AND (observed_at_ns<? OR (observed_at_ns=? AND row_id>?)) SETTINGS mutations_sync=2", provider, ns, ns, id); err != nil {
			return fmt.Errorf("cap quota history: %w", err)
		}
	}
	return nil
}
func (s *Store) QuotaHistory(ctx context.Context, provider string, start, end time.Time) ([]QuotaHistorySample, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrClosed
	}
	rows, err := s.db.Conn().Query(ctx, "SELECT credential_id,window_id,observed_at_ns,remaining_percent,reset_at_ns,period_hours,plan_id,capacity,source FROM "+s.table("analytics_quota_history")+" WHERE provider=? AND observed_at_ns>=? AND observed_at_ns<? ORDER BY observed_at_ns,row_id LIMIT 100000", provider, start.UnixNano(), end.UnixNano())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []QuotaHistorySample{}
	for rows.Next() {
		var item QuotaHistorySample
		var ns int64
		var reset *int64
		if err := rows.Scan(&item.CredentialID, &item.WindowID, &ns, &item.RemainingPercent, &reset, &item.PeriodHours, &item.PlanID, &item.Capacity, &item.Source); err != nil {
			return nil, err
		}
		item.Provider = provider
		item.ObservedAt = time.Unix(0, ns).UTC()
		if reset != nil {
			value := time.Unix(0, *reset).UTC()
			item.ResetAt = &value
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
func validQuotaHistoryFloat(value *float64, maxValue float64) bool {
	return value == nil || !math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= 0 && *value <= maxValue
}
