-- The 15-minute cache is rebuilt by the Go migration hook.  Request IDs are
-- converted from their canonical hex form to a 16-byte BLOB there because
-- SQLite has no portable built-in hex decoder across the supported versions.
CREATE TABLE analytics_15m_compact (
    bucket_start_ns INTEGER NOT NULL,
    bucket_end_ns INTEGER NOT NULL,
    dimension_kind TEXT NOT NULL CHECK(dimension_kind IN ('overall', 'model', 'provider', 'key')),
    dimension_value TEXT NOT NULL,
    first_activity_ns INTEGER NOT NULL,
    last_activity_ns INTEGER NOT NULL,
    proxy_requests INTEGER NOT NULL CHECK(proxy_requests >= 0),
    upstream_attempts INTEGER NOT NULL CHECK(upstream_attempts >= 0),
    succeeded_attempts INTEGER NOT NULL CHECK(succeeded_attempts >= 0),
    failed_attempts INTEGER NOT NULL CHECK(failed_attempts >= 0),
    input_tokens INTEGER NOT NULL CHECK(input_tokens >= 0),
    output_tokens INTEGER NOT NULL CHECK(output_tokens >= 0),
    reasoning_tokens INTEGER NOT NULL CHECK(reasoning_tokens >= 0),
    cached_tokens INTEGER NOT NULL CHECK(cached_tokens >= 0),
    cache_read_tokens INTEGER NOT NULL CHECK(cache_read_tokens >= 0),
    cache_creation_tokens INTEGER NOT NULL CHECK(cache_creation_tokens >= 0),
    total_tokens INTEGER NOT NULL CHECK(total_tokens >= 0),
    generation_time_ms INTEGER NOT NULL CHECK(generation_time_ms >= 0),
    generation_sample_count INTEGER NOT NULL CHECK(generation_sample_count >= 0),
    known_cost_nano INTEGER NOT NULL,
    unpriced_tokens INTEGER NOT NULL CHECK(unpriced_tokens >= 0),
    token_quality TEXT NOT NULL CHECK(token_quality IN ('exact', 'estimated', 'missing')),
    PRIMARY KEY(bucket_start_ns, dimension_kind, dimension_value),
    CHECK(bucket_start_ns < bucket_end_ns),
    CHECK((dimension_kind = 'overall' AND dimension_value = '') OR
          (dimension_kind <> 'overall' AND dimension_value <> ''))
) WITHOUT ROWID;

CREATE INDEX analytics_15m_compact_range_idx
    ON analytics_15m_compact(bucket_start_ns, bucket_end_ns, dimension_kind);
CREATE INDEX analytics_15m_compact_dimension_idx
    ON analytics_15m_compact(dimension_kind, dimension_value, bucket_start_ns);

-- One relation serves every dimensional request count.  A request can have
-- multiple models/providers/keys while retaining exact distinct counts for
-- each dimension, and retries in another bucket remain independent rows.
CREATE TABLE analytics_15m_request_ids (
    bucket_start_ns INTEGER NOT NULL,
    proxy_request_id BLOB NOT NULL CHECK(length(proxy_request_id) = 16),
    model TEXT NOT NULL,
    provider TEXT NOT NULL,
    key_id TEXT NOT NULL,
    PRIMARY KEY(bucket_start_ns, proxy_request_id, model, provider, key_id)
) WITHOUT ROWID;

CREATE INDEX analytics_15m_request_ids_range_idx
    ON analytics_15m_request_ids(bucket_start_ns, proxy_request_id);
CREATE INDEX analytics_15m_request_ids_model_idx
    ON analytics_15m_request_ids(bucket_start_ns, model, proxy_request_id);
CREATE INDEX analytics_15m_request_ids_provider_idx
    ON analytics_15m_request_ids(bucket_start_ns, provider, proxy_request_id);
CREATE INDEX analytics_15m_request_ids_key_idx
    ON analytics_15m_request_ids(bucket_start_ns, key_id, proxy_request_id);
