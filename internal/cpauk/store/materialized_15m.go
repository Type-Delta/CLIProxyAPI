package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

const materialized15mDuration = 15 * time.Minute
const materialized15mDurationNS = int64(materialized15mDuration)

const insertMaterialized15mRequestSQL = `INSERT OR IGNORE INTO analytics_15m_requests (
bucket_start_ns, bucket_end_ns, proxy_request_id, key_id, provider, model, credential_id,
endpoint_class, auth_type, service_tier, succeeded, error_class, status_code, token_quality,
latency_bucket, cache_class, import_batch_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const upsertMaterialized15mSQL = `INSERT INTO analytics_15m (
bucket_start_ns, bucket_end_ns, first_activity_ns, last_activity_ns, key_id, provider, model,
credential_id, endpoint_class, auth_type, service_tier, succeeded, error_class, status_code,
token_quality, latency_bucket, cache_class, import_batch_id, proxy_requests, upstream_attempts,
input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens,
cache_creation_tokens, total_tokens, generation_time_ms, generation_sample_count, known_cost_nano,
unpriced_tokens)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (bucket_start_ns, key_id, provider, model, credential_id, endpoint_class,
auth_type, service_tier, succeeded, error_class, status_code, token_quality, latency_bucket,
cache_class, import_batch_id) DO UPDATE SET
first_activity_ns = MIN(first_activity_ns, excluded.first_activity_ns),
last_activity_ns = MAX(last_activity_ns, excluded.last_activity_ns),
proxy_requests = proxy_requests + excluded.proxy_requests,
upstream_attempts = upstream_attempts + excluded.upstream_attempts,
input_tokens = input_tokens + excluded.input_tokens,
output_tokens = output_tokens + excluded.output_tokens,
reasoning_tokens = reasoning_tokens + excluded.reasoning_tokens,
cached_tokens = cached_tokens + excluded.cached_tokens,
cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens,
cache_creation_tokens = cache_creation_tokens + excluded.cache_creation_tokens,
total_tokens = total_tokens + excluded.total_tokens,
generation_time_ms = generation_time_ms + excluded.generation_time_ms,
generation_sample_count = generation_sample_count + excluded.generation_sample_count,
known_cost_nano = known_cost_nano + excluded.known_cost_nano,
unpriced_tokens = unpriced_tokens + excluded.unpriced_tokens`

const materialized15mRebuildInsertSQL = `WITH classified AS (
SELECT requested_at_ns - ((requested_at_ns % ? + ?) % ?) AS bucket_start_ns, requested_at_ns,
key_id, provider, model, COALESCE(credential_id, '') AS credential_id,
endpoint_class, COALESCE(auth_type, '') AS auth_type,
COALESCE(service_tier_used, service_tier_requested, '') AS service_tier,
succeeded, COALESCE(error_class, '') AS error_class,
COALESCE(upstream_status_code, 0) AS status_code, token_quality,
CASE WHEN latency_ms < 100 THEN '<100ms' WHEN latency_ms < 500 THEN '100-499ms'
WHEN latency_ms < 1000 THEN '500-999ms' ELSE '1000ms+' END AS latency_bucket,
CASE WHEN cache_read_tokens > 0 THEN 'cached' ELSE 'uncached' END AS cache_class,
COALESCE(import_batch_id, '') AS import_batch_id, proxy_request_id,
input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens,
cache_creation_tokens, total_tokens, generation_time_ms,
COALESCE(known_cost_nano, 0) AS known_cost_nano, unpriced_tokens FROM events)
INSERT INTO analytics_15m (
bucket_start_ns, bucket_end_ns, first_activity_ns, last_activity_ns, key_id, provider, model,
credential_id, endpoint_class, auth_type, service_tier, succeeded, error_class, status_code,
token_quality, latency_bucket, cache_class, import_batch_id, proxy_requests, upstream_attempts,
input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens,
cache_creation_tokens, total_tokens, generation_time_ms, generation_sample_count,
known_cost_nano, unpriced_tokens)
SELECT bucket_start_ns, bucket_start_ns + ?, MIN(requested_at_ns), MAX(requested_at_ns),
key_id, provider, model, credential_id, endpoint_class, auth_type, service_tier, succeeded,
error_class, status_code, token_quality, latency_bucket, cache_class, import_batch_id,
COUNT(DISTINCT proxy_request_id), COUNT(*), SUM(input_tokens), SUM(output_tokens),
SUM(reasoning_tokens), SUM(cached_tokens), SUM(cache_read_tokens), SUM(cache_creation_tokens),
SUM(total_tokens), COALESCE(SUM(generation_time_ms), 0), COUNT(generation_time_ms),
SUM(known_cost_nano), SUM(unpriced_tokens)
FROM classified
GROUP BY bucket_start_ns, key_id, provider, model, credential_id, endpoint_class,
auth_type, service_tier, succeeded, error_class, status_code, token_quality, latency_bucket,
cache_class, import_batch_id`

const materialized15mRebuildRequestsSQL = `WITH classified AS (
SELECT requested_at_ns - ((requested_at_ns % ? + ?) % ?) AS bucket_start_ns, proxy_request_id,
key_id, provider, model, COALESCE(credential_id, '') AS credential_id,
endpoint_class, COALESCE(auth_type, '') AS auth_type,
COALESCE(service_tier_used, service_tier_requested, '') AS service_tier,
succeeded, COALESCE(error_class, '') AS error_class,
COALESCE(upstream_status_code, 0) AS status_code, token_quality,
CASE WHEN latency_ms < 100 THEN '<100ms' WHEN latency_ms < 500 THEN '100-499ms'
WHEN latency_ms < 1000 THEN '500-999ms' ELSE '1000ms+' END AS latency_bucket,
CASE WHEN cache_read_tokens > 0 THEN 'cached' ELSE 'uncached' END AS cache_class,
COALESCE(import_batch_id, '') AS import_batch_id FROM events)
INSERT OR IGNORE INTO analytics_15m_requests (
bucket_start_ns, bucket_end_ns, proxy_request_id, key_id, provider, model, credential_id,
endpoint_class, auth_type, service_tier, succeeded, error_class, status_code, token_quality,
latency_bucket, cache_class, import_batch_id)
SELECT bucket_start_ns, bucket_start_ns + ?, proxy_request_id, key_id, provider, model,
credential_id, endpoint_class, auth_type, service_tier, succeeded, error_class, status_code,
token_quality, latency_bucket, cache_class, import_batch_id FROM classified
GROUP BY bucket_start_ns, proxy_request_id, key_id, provider, model, credential_id,
endpoint_class, auth_type, service_tier, succeeded, error_class, status_code, token_quality,
latency_bucket, cache_class, import_batch_id`

type materialized15mKey struct {
	bucketStart int64
	bucketEnd   int64

	keyID        string
	provider     string
	model        string
	credentialID string
	endpoint     string
	authType     string
	serviceTier  string
	succeeded    bool
	errorClass   string
	statusCode   int
	tokenQuality string
	latency      string
	cacheClass   string
	importBatch  string
}

func materialized15mKeyFor(event model.Event, importBatchID string) materialized15mKey {
	requestedNS := event.RequestedAt.UTC().UnixNano()
	bucketNS := requestedNS / materialized15mDurationNS
	if requestedNS < 0 && requestedNS%materialized15mDurationNS != 0 {
		bucketNS--
	}
	start := time.Unix(0, bucketNS*materialized15mDurationNS).UTC()
	key := materialized15mKey{
		bucketStart: start.UnixNano(), bucketEnd: start.Add(materialized15mDuration).UnixNano(),
		keyID: event.KeyID, provider: event.Provider, model: event.Model, endpoint: event.EndpointClass,
		succeeded: event.Succeeded, tokenQuality: string(event.Tokens.Quality),
		latency: latencyBucket(event.LatencyMS), cacheClass: cacheClass(event.Tokens.CacheRead), importBatch: importBatchID,
	}
	if event.CredentialID != nil {
		key.credentialID = *event.CredentialID
	}
	if event.AuthType != nil {
		key.authType = *event.AuthType
	}
	if event.ServiceTierUsed != nil {
		key.serviceTier = *event.ServiceTierUsed
	} else if event.ServiceTierRequested != nil {
		key.serviceTier = *event.ServiceTierRequested
	}
	if event.ErrorClass != nil {
		key.errorClass = *event.ErrorClass
	}
	if event.UpstreamStatusCode != nil {
		key.statusCode = *event.UpstreamStatusCode
	}
	return key
}

func latencyBucket(latencyMS int64) string {
	switch {
	case latencyMS < 100:
		return "<100ms"
	case latencyMS < 500:
		return "100-499ms"
	case latencyMS < 1000:
		return "500-999ms"
	default:
		return "1000ms+"
	}
}

func cacheClass(cacheReadTokens int64) string {
	if cacheReadTokens > 0 {
		return "cached"
	}
	return "uncached"
}

func (s *SQLiteStore) upsertMaterialized15mTx(ctx context.Context, tx *sql.Tx, event model.Event, knownCost int64, unpricedTokens int64, importBatchID string) error {
	key := materialized15mKeyFor(event, importBatchID)
	var generationTimeMS, generationSamples int64
	if event.GenerationTimeMS != nil {
		generationTimeMS, generationSamples = *event.GenerationTimeMS, 1
	}
	requestResult, err := tx.ExecContext(ctx, insertMaterialized15mRequestSQL,
		key.bucketStart, key.bucketEnd, event.ProxyRequestID, key.keyID, key.provider, key.model,
		key.credentialID, key.endpoint, key.authType, key.serviceTier, key.succeeded, key.errorClass,
		key.statusCode, key.tokenQuality, key.latency, key.cacheClass, key.importBatch)
	if err != nil {
		return fmt.Errorf("insert 15-minute request identity: %w", err)
	}
	proxyRequests, err := requestResult.RowsAffected()
	if err != nil {
		return fmt.Errorf("count 15-minute request identity: %w", err)
	}
	_, err = tx.ExecContext(ctx, upsertMaterialized15mSQL,
		key.bucketStart, key.bucketEnd, event.RequestedAt.UnixNano(), event.RequestedAt.UnixNano(),
		key.keyID, key.provider, key.model, key.credentialID, key.endpoint, key.authType,
		key.serviceTier, key.succeeded, key.errorClass, key.statusCode, key.tokenQuality, key.latency,
		key.cacheClass, key.importBatch, proxyRequests, 1, event.Tokens.Input, event.Tokens.Output,
		event.Tokens.Reasoning, event.Tokens.Cached, event.Tokens.CacheRead, event.Tokens.CacheCreation,
		event.Tokens.Total, generationTimeMS, generationSamples, knownCost, unpricedTokens)
	if err != nil {
		return fmt.Errorf("upsert 15-minute aggregate: %w", err)
	}
	return nil
}

// Rebuild15MinuteAggregates replaces all materialized rows from the durable
// event table. It is intentionally explicit so maintenance and tests can
// repair an interrupted or manually altered aggregate state.
func (s *SQLiteStore) Rebuild15MinuteAggregates(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rebuild15mLocked(ctx)
}

// rebuild15m is kept as a small package-local hook for store maintenance tests
// and callers that already use the concise maintenance naming convention.
func (s *SQLiteStore) rebuild15m(ctx context.Context) error {
	return s.Rebuild15MinuteAggregates(ctx)
}

func (s *SQLiteStore) rebuild15mLocked(ctx context.Context) error {
	if s.db == nil {
		return ErrClosed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin 15-minute aggregate rebuild: %w", err)
	}
	rollback := func(cause error) error {
		_ = tx.Rollback()
		return cause
	}
	if err := rebuildMaterialized15mTx(ctx, tx); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit 15-minute aggregate rebuild: %w", err)
	}
	return nil
}

func rebuildMaterialized15mTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_15m_requests"); err != nil {
		return fmt.Errorf("clear 15-minute request identities: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_15m"); err != nil {
		return fmt.Errorf("clear 15-minute aggregates: %w", err)
	}
	if _, err := tx.ExecContext(ctx, materialized15mRebuildInsertSQL,
		materialized15mDurationNS, materialized15mDurationNS, materialized15mDurationNS,
		materialized15mDurationNS); err != nil {
		return fmt.Errorf("backfill 15-minute aggregates: %w", err)
	}
	if _, err := tx.ExecContext(ctx, materialized15mRebuildRequestsSQL,
		materialized15mDurationNS, materialized15mDurationNS, materialized15mDurationNS,
		materialized15mDurationNS); err != nil {
		return fmt.Errorf("backfill 15-minute request identities: %w", err)
	}
	return nil
}

// rebuildMaterialized15mBucketsTx recomputes only buckets named in the
// transaction-local table. Callers use it after deleting or repricing events,
// so the aggregate never exposes a partial delta.
func rebuildMaterialized15mBucketsTx(ctx context.Context, tx *sql.Tx, bucketStarts []int64) error {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE materialized_15m_rebuild_buckets (
bucket_start_ns INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create 15-minute aggregate bucket set: %w", err)
	}
	defer func() { _, _ = tx.ExecContext(ctx, "DROP TABLE materialized_15m_rebuild_buckets") }()
	for _, bucketStart := range bucketStarts {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO materialized_15m_rebuild_buckets(bucket_start_ns) VALUES (?)`, bucketStart); err != nil {
			return fmt.Errorf("mark 15-minute aggregate bucket: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM analytics_15m_requests
WHERE bucket_start_ns IN (SELECT bucket_start_ns FROM materialized_15m_rebuild_buckets)`); err != nil {
		return fmt.Errorf("clear selected 15-minute request identities: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM analytics_15m
WHERE bucket_start_ns IN (SELECT bucket_start_ns FROM materialized_15m_rebuild_buckets)`); err != nil {
		return fmt.Errorf("clear selected 15-minute aggregates: %w", err)
	}
	aggregateSQL := strings.Replace(materialized15mRebuildInsertSQL, "\nFROM classified\n", "\nFROM classified\nWHERE bucket_start_ns IN (SELECT bucket_start_ns FROM materialized_15m_rebuild_buckets)\n", 1)
	if _, err := tx.ExecContext(ctx, aggregateSQL,
		materialized15mDurationNS, materialized15mDurationNS, materialized15mDurationNS,
		materialized15mDurationNS); err != nil {
		return fmt.Errorf("rebuild selected 15-minute aggregates: %w", err)
	}
	requestSQL := strings.Replace(materialized15mRebuildRequestsSQL, "\nFROM classified\n", "\nFROM classified\nWHERE bucket_start_ns IN (SELECT bucket_start_ns FROM materialized_15m_rebuild_buckets)\n", 1)
	if _, err := tx.ExecContext(ctx, requestSQL,
		materialized15mDurationNS, materialized15mDurationNS, materialized15mDurationNS,
		materialized15mDurationNS); err != nil {
		return fmt.Errorf("rebuild selected 15-minute request identities: %w", err)
	}
	return nil
}
