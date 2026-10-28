package chstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/store"
)

func (s *Store) c4bImportControl(ctx context.Context, legacy *store.SQLiteStore, db *sql.DB, counts map[string]int64) error {
	snapshot, err := legacy.PricingSnapshot(ctx)
	if err != nil {
		return err
	}
	providers, _, err := legacy.CatalogProviders(ctx)
	if err != nil {
		return err
	}
	values := map[string]json.RawMessage{}
	values["snapshot"], err = json.Marshal(snapshot)
	if err != nil {
		return err
	}
	values["providers"], err = json.Marshal(providers)
	if err != nil {
		return err
	}
	current, err := s.loadControl(ctx, "pricing")
	if err != nil {
		return err
	}
	// An existing snapshot includes acknowledged online changes. Import only
	// seeds an untouched namespace, including when resuming after a failure.
	if len(current) == 0 {
		if err = s.replaceControl(ctx, "pricing", values); err != nil {
			return err
		}
		s.config.PriceBook.Rules = clonePricingRules(snapshot.Rules)
		s.config.PriceBook.KiroUSDPerCredit = snapshot.KiroUSDPerCredit
	}

	counts["pricing_rules"] = int64(len(snapshot.Rules))
	counts["pricing_catalog_providers"] = int64(len(providers))
	quotas, err := legacy.ProviderQuotaSnapshots(ctx)
	if err != nil {
		return err
	}
	values = map[string]json.RawMessage{}
	for _, item := range quotas {
		key, _ := json.Marshal([]string{item.Provider, item.CredentialID, item.Model})
		raw, errMarshal := json.Marshal(item)
		if errMarshal != nil {
			return errMarshal
		}
		values[string(key)] = raw
	}
	if err = s.replaceControl(ctx, "provider_quota", values); err != nil {
		return err
	}
	counts["provider_quota_snapshots"] = int64(len(values))
	values = map[string]json.RawMessage{}
	rows, err := db.QueryContext(ctx, "SELECT key_id,status,updated_at_ns FROM key_lifecycle")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var status model.KeyStatus
		var ns int64
		if err = rows.Scan(&id, &status, &ns); err != nil {
			_ = rows.Close()
			return err
		}
		values[id], err = json.Marshal(keyLifecycleEntry{status, time.Unix(0, ns).UTC()})
		if err != nil {
			_ = rows.Close()
			return err
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if err = s.replaceControl(ctx, "key_lifecycle", values); err != nil {
		return err
	}
	counts["key_lifecycle"] = int64(len(values))
	quotaRows := make([]quotaHistoryRow, 0, 1000)
	var quotaCount int64
	rows, err = db.QueryContext(ctx, "SELECT provider,credential_id,window_id,observed_at_ns,remaining_percent,reset_at_ns,period_hours,plan_id,capacity,source FROM quota_history")
	if err != nil {
		return err
	}
	for rows.Next() {
		var item QuotaHistorySample
		var ns int64
		var reset sql.NullInt64
		var period, capacity sql.NullFloat64
		var plan sql.NullString
		if err = rows.Scan(&item.Provider, &item.CredentialID, &item.WindowID, &ns, &item.RemainingPercent, &reset, &period, &plan, &capacity, &item.Source); err != nil {
			_ = rows.Close()
			return err
		}
		item.ObservedAt = time.Unix(0, ns).UTC()
		item.ResetAt = c4bNullableTime(reset)
		item.PlanID = plan.String
		if period.Valid {
			item.PeriodHours = &period.Float64
		}
		if capacity.Valid {
			item.Capacity = &capacity.Float64
		}
		quotaRows = append(quotaRows, item)
		quotaCount++
		if len(quotaRows) == 1000 {
			if err = s.appendQuotaHistoryRows(ctx, quotaRows); err != nil {
				_ = rows.Close()
				return err
			}
			quotaRows = quotaRows[:0]
			if err = s.yieldLegacyImport(ctx); err != nil {
				_ = rows.Close()
				return err
			}
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if err = s.appendQuotaHistoryRows(ctx, quotaRows); err != nil {
		return err
	}
	counts["quota_history"] = quotaCount
	values = map[string]json.RawMessage{}
	rows, err = db.QueryContext(ctx, "SELECT batch_id,source_kind,source_fingerprint,source_offset,chunk,digest,completed,rows_read,transformed,inserted,skipped,rejected,updated_at_ns FROM import_checkpoints")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, digest string
		var item importCheckpoint
		var ns int64
		if err = rows.Scan(&id, &item.SourceKind, &item.Fingerprint, &item.Offset, &item.Chunk, &digest, &item.Completed, &item.Counters[0], &item.Counters[1], &item.Counters[2], &item.Counters[3], &item.Counters[4], &ns); err != nil {
			_ = rows.Close()
			return err
		}
		item.Checkpoint = []byte(digest)
		item.UpdatedAt = time.Unix(0, ns).UTC()
		values[id], err = json.Marshal(item)
		if err != nil {
			_ = rows.Close()
			return err
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if err = s.replaceControl(ctx, "import_checkpoints", values); err != nil {
		return err
	}
	counts["import_checkpoints"] = int64(len(values))
	// Keep detailed source-table counts in the job even though control rows are
	// published through fewer atomic ClickHouse namespaces.
	for _, table := range []string{"pricing_rules", "pricing_manual_rules", "pricing_catalog_rules", "pricing_provenance", "pricing_catalog_provenance", "pricing_catalog_bindings", "pricing_catalog_providers", "analytics_metadata", "retention_state"} {
		var count int64
		if err = db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			return err
		}
		counts[table] = count
	}
	return nil
}
