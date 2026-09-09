package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

const insertEventSQL = `INSERT OR IGNORE INTO events (
attempt_id, schema_version, proxy_request_id, request_id_quality, key_id, requested_at_ns,
provider, executor_type, model, requested_alias, endpoint_class, auth_type, credential_id,
credential_id_algorithm, succeeded, upstream_status_code, error_class, latency_ms,
time_to_first_token_ms, service_tier_requested, service_tier_used, generated,
input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens,
cache_creation_tokens, total_tokens, accounting_schema, token_quality, known_cost_nano,
unpriced_tokens, price_rule_id, price_source, import_batch_id, generation_time_ms,
client_method, client_path, received_at_ns, upstream_method, upstream_url, upstream_sent_at_ns,
upstream_usage_raw, upstream_error_body, proxy_status_code, proxy_error, responded_at_ns, first_token_latency_ms, provider_latency_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// patchProxyResponseSQL records what the proxy returned to the client for every
// attempt of one request. The first patch wins so retries cannot rewrite history.
const patchProxyResponseSQL = `UPDATE events SET proxy_status_code = ?, proxy_error = ?, responded_at_ns = ?
WHERE proxy_request_id = ? AND proxy_status_code IS NULL AND responded_at_ns IS NULL`

func (s *SQLiteStore) WriteBatch(ctx context.Context, events []model.Event) error {
	_, err := s.writeBatch(ctx, events, "")
	return err
}

func (s *SQLiteStore) WriteImportBatch(ctx context.Context, events []model.Event, batchID string) (int64, error) {
	if batchID == "" {
		return 0, fmt.Errorf("import batch ID is required")
	}
	return s.writeBatch(ctx, events, batchID)
}

func (s *SQLiteStore) writeBatch(ctx context.Context, events []model.Event, batchID string) (int64, error) {
	if len(events) == 0 {
		return 0, nil
	}
	for index := range events {
		if err := events[index].Validate(); err != nil {
			return 0, fmt.Errorf("validate event %d: %w", index, err)
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return 0, ErrClosed
	}
	if err := s.checkQuota(int64(len(events) * model.MaxEventBytes)); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin analytics batch: %w", err)
	}
	statement, err := tx.PrepareContext(ctx, insertEventSQL)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("prepare analytics batch: %w", err)
	}
	defer func() { _ = statement.Close() }()
	var inserted int64
	for index := range events {
		price, err := s.price(events[index])
		if err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("price event %d: %w", index, err)
		}
		var knownCost any
		if price.KnownCost != nil {
			knownCost = int64(*price.KnownCost)
		}
		result, err := statement.ExecContext(ctx, eventArguments(events[index], knownCost, price.UnpricedTokens, price.RuleID, price.Source, nullString(batchID))...)
		if err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("insert analytics event %d: %w", index, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("count inserted analytics event %d: %w", index, err)
		}
		inserted += count
	}
	if err := s.checkQuota(int64(len(events) * model.MaxEventBytes)); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit analytics batch: %w", err)
	}
	return inserted, nil
}

func eventArguments(event model.Event, knownCost any, unpriced int64, ruleID, source string, batchID any) []any {
	return []any{
		event.AttemptID, event.SchemaVersion, event.ProxyRequestID, string(event.RequestIDQuality), event.KeyID, event.RequestedAt.UnixNano(),
		event.Provider, event.ExecutorType, event.Model, nullStringPointer(event.RequestedAlias), event.EndpointClass,
		nullStringPointer(event.AuthType), nullStringPointer(event.CredentialID), nullStringPointer(event.CredentialIDAlgorithm),
		event.Succeeded, nullIntPointer(event.UpstreamStatusCode), nullStringPointer(event.ErrorClass), event.LatencyMS,
		nullInt64Pointer(event.TimeToFirstTokenMS), nullStringPointer(event.ServiceTierRequested), nullStringPointer(event.ServiceTierUsed), event.Generated,
		event.Tokens.Input, event.Tokens.Output, event.Tokens.Reasoning, event.Tokens.Cached, event.Tokens.CacheRead,
		event.Tokens.CacheCreation, event.Tokens.Total, event.Tokens.Schema, string(event.Tokens.Quality), knownCost,
		unpriced, nullString(ruleID), nullString(source), batchID, nullInt64Pointer(event.GenerationTimeMS),
		nullStringPointer(event.ClientMethod), nullStringPointer(event.ClientPath), nullTimePointer(event.ReceivedAt),
		nullStringPointer(event.UpstreamMethod), nullStringPointer(event.UpstreamURL), nullTimePointer(event.UpstreamSentAt),
		nullRawJSONPointer(event.UpstreamUsageRaw), nullStringPointer(event.UpstreamErrorBody),
		nullIntPointer(event.ProxyStatusCode), nullStringPointer(event.ProxyError), nullTimePointer(event.RespondedAt), nullInt64Pointer(event.FirstTokenLatencyMS), nullInt64Pointer(event.ProviderLatencyMS),
	}
}

func nullTimePointer(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return value.UnixNano()
}

func nullRawJSONPointer(value *model.RawJSON) any {
	if value == nil || *value == "" {
		return nil
	}
	return string(*value)
}

// WriteBatchWithPatches inserts events and then applies proxy response patches
// in the same transaction, in queue order, so a patch that follows its event
// in the collector queue always finds the row.
func (s *SQLiteStore) WriteBatchWithPatches(ctx context.Context, events []model.Event, patches []model.ProxyResponsePatch) error {
	if len(events) > 0 {
		if _, err := s.writeBatch(ctx, events, ""); err != nil {
			return err
		}
	}
	if len(patches) == 0 {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return ErrClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin analytics patch batch: %w", err)
	}
	statement, err := tx.PrepareContext(ctx, patchProxyResponseSQL)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare analytics patch batch: %w", err)
	}
	defer func() { _ = statement.Close() }()
	for index, patch := range patches {
		if !model.IsCorrelationID(patch.ProxyRequestID) || patch.StatusCode < 100 || patch.StatusCode > 599 {
			continue
		}
		respondedAt := patch.RespondedAt
		if respondedAt.IsZero() {
			respondedAt = time.Now()
		}
		if _, err := statement.ExecContext(ctx, patch.StatusCode, nullString(patch.Error), respondedAt.UnixNano(), patch.ProxyRequestID); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("patch analytics event %d: %w", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit analytics patch batch: %w", err)
	}
	return nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullStringPointer(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullIntPointer(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullInt64Pointer(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func scanNullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}
