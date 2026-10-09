package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

const materialized15mDuration = 15 * time.Minute
const materialized15mDurationNS = int64(materialized15mDuration)

// The compact cache stores additive metrics at the four dimensions used by
// common reads. Distinct request counts live in analytics_15m_request_ids so
// retries and requests spanning multiple dimensional rows remain exact.
const compactAggregateSelect = `WITH classified AS (
SELECT e.requested_at_ns - ((e.requested_at_ns % ? + ?) % ?) AS bucket_start_ns,
e.requested_at_ns,e.proxy_request_id,e.key_id,e.provider,e.model,
e.succeeded,e.input_tokens,e.output_tokens,e.reasoning_tokens,e.cached_tokens,
e.cache_read_tokens,e.cache_creation_tokens,e.total_tokens,e.generation_time_ms,
COALESCE(e.known_cost_nano,0) AS known_cost_nano,e.unpriced_tokens,e.token_quality
FROM events e
), dimensions AS (
SELECT bucket_start_ns,requested_at_ns,proxy_request_id,'overall' AS dimension_kind,
'' AS dimension_value,succeeded,input_tokens,output_tokens,reasoning_tokens,
cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,
generation_time_ms,known_cost_nano,unpriced_tokens,token_quality FROM classified
UNION ALL
SELECT bucket_start_ns,requested_at_ns,proxy_request_id,'model',model,succeeded,
input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,
cache_creation_tokens,total_tokens,generation_time_ms,known_cost_nano,
unpriced_tokens,token_quality FROM classified
UNION ALL
SELECT bucket_start_ns,requested_at_ns,proxy_request_id,'provider',provider,succeeded,
input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,
cache_creation_tokens,total_tokens,generation_time_ms,known_cost_nano,
unpriced_tokens,token_quality FROM classified
UNION ALL
SELECT bucket_start_ns,requested_at_ns,proxy_request_id,'key',key_id,succeeded,
input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,
cache_creation_tokens,total_tokens,generation_time_ms,known_cost_nano,
unpriced_tokens,token_quality FROM classified
)
INSERT INTO analytics_15m_compact (
bucket_start_ns,bucket_end_ns,dimension_kind,dimension_value,first_activity_ns,
last_activity_ns,proxy_requests,upstream_attempts,succeeded_attempts,failed_attempts,
input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,
cache_creation_tokens,total_tokens,generation_time_ms,generation_sample_count,
known_cost_nano,unpriced_tokens,token_quality)
SELECT bucket_start_ns,bucket_start_ns+?,dimension_kind,dimension_value,
MIN(requested_at_ns),MAX(requested_at_ns),COUNT(DISTINCT proxy_request_id),COUNT(*),
SUM(CASE WHEN succeeded THEN 1 ELSE 0 END),SUM(CASE WHEN succeeded THEN 0 ELSE 1 END),
SUM(input_tokens),SUM(output_tokens),SUM(reasoning_tokens),SUM(cached_tokens),
SUM(cache_read_tokens),SUM(cache_creation_tokens),SUM(total_tokens),
COALESCE(SUM(generation_time_ms),0),COUNT(generation_time_ms),SUM(known_cost_nano),
SUM(unpriced_tokens),
CASE WHEN SUM(CASE WHEN token_quality='missing' THEN 1 ELSE 0 END)>0 THEN 'missing'
WHEN SUM(CASE WHEN token_quality='estimated' THEN 1 ELSE 0 END)>0 THEN 'estimated'
ELSE 'exact' END
FROM dimensions
GROUP BY bucket_start_ns,dimension_kind,dimension_value`

const compactRequestInsertSQL = `INSERT OR IGNORE INTO analytics_15m_request_ids
(bucket_start_ns,proxy_request_id,model,provider,key_id) VALUES (?,?,?,?,?)`

func materialized15mBucketStart(requestedNS int64) int64 {
	bucket := requestedNS / materialized15mDurationNS
	if requestedNS < 0 && requestedNS%materialized15mDurationNS != 0 {
		bucket--
	}
	return bucket * materialized15mDurationNS
}

func compactRequestID(value string) ([]byte, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 16 {
		return nil, fmt.Errorf("invalid proxy request ID %q", value)
	}
	return decoded, nil
}

func (s *SQLiteStore) Rebuild15MinuteAggregates(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rebuild15mLocked(ctx)
}

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
	bucketStarts, err := bucketStartsForEvents(ctx, tx, "1=1")
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("find 15-minute aggregate rebuild buckets: %w", err)
	}
	if err := rebuildMaterialized15mTx(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := touchHistoryGenerationsTx(ctx, tx, bucketStarts); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("update history generations after aggregate rebuild: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit 15-minute aggregate rebuild: %w", err)
	}
	return nil
}

func rebuildMaterialized15mTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_15m_compact"); err != nil {
		return fmt.Errorf("clear compact 15-minute aggregates: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_15m_request_ids"); err != nil {
		return fmt.Errorf("clear compact 15-minute request identities: %w", err)
	}
	if _, err := tx.ExecContext(ctx, compactAggregateSelect,
		materialized15mDurationNS, materialized15mDurationNS, materialized15mDurationNS,
		materialized15mDurationNS); err != nil {
		return fmt.Errorf("backfill compact 15-minute aggregates: %w", err)
	}
	if err := insertCompactRequestIDs(ctx, tx, false); err != nil {
		return err
	}
	return nil
}

func rebuildMaterialized15mBucketsTx(ctx context.Context, tx *sql.Tx, bucketStarts []int64) error {
	if len(bucketStarts) == 0 {
		return nil
	}
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM analytics_15m_compact
WHERE bucket_start_ns IN (SELECT bucket_start_ns FROM materialized_15m_rebuild_buckets)`); err != nil {
		return fmt.Errorf("clear selected compact 15-minute aggregates: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM analytics_15m_request_ids
WHERE bucket_start_ns IN (SELECT bucket_start_ns FROM materialized_15m_rebuild_buckets)`); err != nil {
		return fmt.Errorf("clear selected compact 15-minute request identities: %w", err)
	}
	if err := insertCompactAggregatesForBuckets(ctx, tx); err != nil {
		return err
	}
	if err := insertCompactRequestIDs(ctx, tx, true); err != nil {
		return err
	}
	return nil
}

func insertCompactAggregatesForBuckets(ctx context.Context, tx *sql.Tx) error {
	statement := `WITH classified AS (
SELECT b.bucket_start_ns,e.requested_at_ns,e.proxy_request_id,e.key_id,e.provider,e.model,
e.succeeded,e.input_tokens,e.output_tokens,e.reasoning_tokens,e.cached_tokens,
e.cache_read_tokens,e.cache_creation_tokens,e.total_tokens,e.generation_time_ms,
COALESCE(e.known_cost_nano,0) AS known_cost_nano,e.unpriced_tokens,e.token_quality
FROM events e JOIN materialized_15m_rebuild_buckets b
ON e.requested_at_ns >= b.bucket_start_ns
AND e.requested_at_ns < b.bucket_start_ns + ?
), dimensions AS (
SELECT bucket_start_ns,requested_at_ns,proxy_request_id,'overall' AS dimension_kind,
'' AS dimension_value,succeeded,input_tokens,output_tokens,reasoning_tokens,
cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,
generation_time_ms,known_cost_nano,unpriced_tokens,token_quality FROM classified
UNION ALL SELECT bucket_start_ns,requested_at_ns,proxy_request_id,'model',model,succeeded,
input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,
cache_creation_tokens,total_tokens,generation_time_ms,known_cost_nano,unpriced_tokens,token_quality FROM classified
UNION ALL SELECT bucket_start_ns,requested_at_ns,proxy_request_id,'provider',provider,succeeded,
input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,
cache_creation_tokens,total_tokens,generation_time_ms,known_cost_nano,unpriced_tokens,token_quality FROM classified
UNION ALL SELECT bucket_start_ns,requested_at_ns,proxy_request_id,'key',key_id,succeeded,
input_tokens,output_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,
cache_creation_tokens,total_tokens,generation_time_ms,known_cost_nano,unpriced_tokens,token_quality FROM classified
)
INSERT INTO analytics_15m_compact (
bucket_start_ns,bucket_end_ns,dimension_kind,dimension_value,first_activity_ns,last_activity_ns,
proxy_requests,upstream_attempts,succeeded_attempts,failed_attempts,input_tokens,output_tokens,
reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,
generation_time_ms,generation_sample_count,known_cost_nano,unpriced_tokens,token_quality)
SELECT bucket_start_ns,bucket_start_ns+?,dimension_kind,dimension_value,MIN(requested_at_ns),MAX(requested_at_ns),
COUNT(DISTINCT proxy_request_id),COUNT(*),SUM(CASE WHEN succeeded THEN 1 ELSE 0 END),
SUM(CASE WHEN succeeded THEN 0 ELSE 1 END),SUM(input_tokens),SUM(output_tokens),SUM(reasoning_tokens),
SUM(cached_tokens),SUM(cache_read_tokens),SUM(cache_creation_tokens),SUM(total_tokens),
COALESCE(SUM(generation_time_ms),0),COUNT(generation_time_ms),SUM(known_cost_nano),SUM(unpriced_tokens),
CASE WHEN SUM(CASE WHEN token_quality='missing' THEN 1 ELSE 0 END)>0 THEN 'missing'
WHEN SUM(CASE WHEN token_quality='estimated' THEN 1 ELSE 0 END)>0 THEN 'estimated' ELSE 'exact' END
FROM dimensions GROUP BY bucket_start_ns,dimension_kind,dimension_value`
	if _, err := tx.ExecContext(ctx, statement, materialized15mDurationNS, materialized15mDurationNS); err != nil {
		return fmt.Errorf("rebuild selected compact 15-minute aggregates: %w", err)
	}
	return nil
}

func insertCompactRequestIDs(ctx context.Context, tx *sql.Tx, selected bool) error {
	query := `SELECT e.requested_at_ns,e.proxy_request_id,e.model,e.provider,e.key_id FROM events e`
	arguments := []any{}
	if selected {
		query = `SELECT b.bucket_start_ns,e.proxy_request_id,e.model,e.provider,e.key_id
FROM events e JOIN materialized_15m_rebuild_buckets b
ON e.requested_at_ns >= b.bucket_start_ns AND e.requested_at_ns < b.bucket_start_ns + ?`
		arguments = append(arguments, materialized15mDurationNS)
	}
	rows, err := tx.QueryContext(ctx, query, arguments...)
	if err != nil {
		return fmt.Errorf("read compact request identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	statement, err := tx.PrepareContext(ctx, compactRequestInsertSQL)
	if err != nil {
		return fmt.Errorf("prepare compact request identities: %w", err)
	}
	defer func() { _ = statement.Close() }()
	for rows.Next() {
		var requestedNS int64
		var requestID, modelName, provider, keyID string
		if err := rows.Scan(&requestedNS, &requestID, &modelName, &provider, &keyID); err != nil {
			return fmt.Errorf("scan compact request identity: %w", err)
		}
		requestBlob, err := compactRequestID(requestID)
		if err != nil {
			return err
		}
		if _, err := statement.ExecContext(ctx, materialized15mBucketStart(requestedNS), requestBlob, modelName, provider, keyID); err != nil {
			return fmt.Errorf("insert compact request identity: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read compact request identities: %w", err)
	}
	return nil
}

func bucketStartsForEvents(ctx context.Context, tx *sql.Tx, where string, arguments ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT requested_at_ns - ((requested_at_ns % ? + ?) % ?)
FROM events WHERE `+where, append([]any{materialized15mDurationNS, materialized15mDurationNS, materialized15mDurationNS}, arguments...)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]int64, 0)
	for rows.Next() {
		var bucket int64
		if err := rows.Scan(&bucket); err != nil {
			return nil, err
		}
		result = append(result, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
