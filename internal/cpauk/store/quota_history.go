package store

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/backend"
	"math"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

// QuotaHistorySample holds sanitized identity and quota values, never raw credentials.
type QuotaHistorySample = backend.QuotaHistorySample

// RecordQuotaHistory limits each credential/window to one observation per five minutes.
// It prunes after 90 days and caps each provider at 100,000 rows on each write.
func (s *SQLiteStore) RecordQuotaHistory(ctx context.Context, samples []QuotaHistorySample, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return ErrClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `DELETE FROM quota_history WHERE observed_at_ns < ?`, now.Add(-90*24*time.Hour).UnixNano()); err != nil {
		return err
	}
	providers := map[string]bool{}
	for _, sample := range samples {
		if !model.IsFullKeyID(sample.CredentialID) || sample.Provider == "" || len(sample.Provider) > 64 || sample.WindowID == "" || len(sample.WindowID) > 64 || len(sample.PlanID) > 64 || (sample.Source != "cpa_auto" && sample.Source != "cpamc") || math.IsNaN(sample.RemainingPercent) || math.IsInf(sample.RemainingPercent, 0) || sample.RemainingPercent < 0 || sample.RemainingPercent > 100 || !validQuotaHistoryFloat(sample.Capacity, 1e12) || !validQuotaHistoryFloat(sample.PeriodHours, 24*366) {
			return fmt.Errorf("invalid quota history sample")
		}
		providers[sample.Provider] = true
		var last sql.NullInt64
		if err = tx.QueryRowContext(ctx, `SELECT MAX(observed_at_ns) FROM quota_history WHERE provider=? AND credential_id=? AND window_id=?`, sample.Provider, sample.CredentialID, sample.WindowID).Scan(&last); err != nil {
			return err
		}
		if last.Valid && now.UnixNano()-last.Int64 < int64(5*time.Minute) {
			continue
		}
		var reset any
		if sample.ResetAt != nil {
			reset = sample.ResetAt.UnixNano()
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO quota_history(provider,credential_id,window_id,observed_at_ns,remaining_percent,reset_at_ns,period_hours,plan_id,capacity,source) VALUES(?,?,?,?,?,?,?,?,?,?)`, sample.Provider, sample.CredentialID, sample.WindowID, now.UnixNano(), sample.RemainingPercent, reset, sample.PeriodHours, sample.PlanID, sample.Capacity, sample.Source); err != nil {
			return fmt.Errorf("insert quota history: %w", err)
		}
	}
	for provider := range providers {
		if _, err = tx.ExecContext(ctx, `DELETE FROM quota_history WHERE rowid IN (SELECT rowid FROM quota_history WHERE provider=? ORDER BY observed_at_ns DESC LIMIT -1 OFFSET 100000)`, provider); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLiteStore) QuotaHistory(ctx context.Context, provider string, start, end time.Time) ([]QuotaHistorySample, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrClosed
	}
	rows, err := s.db.QueryContext(ctx, `SELECT credential_id,window_id,observed_at_ns,remaining_percent,reset_at_ns,period_hours,plan_id,capacity,source FROM quota_history WHERE provider=? AND observed_at_ns>=? AND observed_at_ns<? ORDER BY observed_at_ns LIMIT 100000`, provider, start.UnixNano(), end.UnixNano())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []QuotaHistorySample{}
	for rows.Next() {
		var sample QuotaHistorySample
		var at int64
		var reset sql.NullInt64
		var period, capacity sql.NullFloat64
		var plan sql.NullString
		if err := rows.Scan(&sample.CredentialID, &sample.WindowID, &at, &sample.RemainingPercent, &reset, &period, &plan, &capacity, &sample.Source); err != nil {
			return nil, err
		}
		sample.Provider = provider
		sample.ObservedAt = time.Unix(0, at).UTC()
		if reset.Valid {
			value := time.Unix(0, reset.Int64).UTC()
			sample.ResetAt = &value
		}
		if period.Valid {
			sample.PeriodHours = &period.Float64
		}
		if capacity.Valid {
			sample.Capacity = &capacity.Float64
		}
		sample.PlanID = plan.String
		result = append(result, sample)
	}
	return result, rows.Err()
}

func validQuotaHistoryFloat(value *float64, maxValue float64) bool {
	return value == nil || !math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= 0 && *value <= maxValue
}
