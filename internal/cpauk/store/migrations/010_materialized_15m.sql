CREATE TABLE analytics_15m (
    bucket_start_ns INTEGER NOT NULL,
    bucket_end_ns INTEGER NOT NULL,
    first_activity_ns INTEGER NOT NULL,
    last_activity_ns INTEGER NOT NULL,
    key_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    credential_id TEXT NOT NULL,
    endpoint_class TEXT NOT NULL,
    auth_type TEXT NOT NULL,
    service_tier TEXT NOT NULL,
    succeeded INTEGER NOT NULL CHECK(succeeded IN (0, 1)),
    error_class TEXT NOT NULL,
    status_code INTEGER NOT NULL,
    token_quality TEXT NOT NULL,
    latency_bucket TEXT NOT NULL,
    cache_class TEXT NOT NULL,
    import_batch_id TEXT NOT NULL,
    proxy_requests INTEGER NOT NULL CHECK(proxy_requests >= 0),
    upstream_attempts INTEGER NOT NULL CHECK(upstream_attempts >= 0),
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
    PRIMARY KEY (bucket_start_ns, key_id, provider, model, credential_id, endpoint_class,
        auth_type, service_tier, succeeded, error_class, status_code, token_quality,
        latency_bucket, cache_class, import_batch_id),
    CHECK(bucket_start_ns < bucket_end_ns)
);

CREATE INDEX analytics_15m_range_idx ON analytics_15m(bucket_start_ns, bucket_end_ns);
CREATE INDEX analytics_15m_key_range_idx ON analytics_15m(key_id, bucket_start_ns);

CREATE TABLE analytics_15m_requests (
    bucket_start_ns INTEGER NOT NULL,
    bucket_end_ns INTEGER NOT NULL,
    proxy_request_id TEXT NOT NULL CHECK(length(proxy_request_id) = 32),
    key_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    credential_id TEXT NOT NULL,
    endpoint_class TEXT NOT NULL,
    auth_type TEXT NOT NULL,
    service_tier TEXT NOT NULL,
    succeeded INTEGER NOT NULL CHECK(succeeded IN (0, 1)),
    error_class TEXT NOT NULL,
    status_code INTEGER NOT NULL,
    token_quality TEXT NOT NULL,
    latency_bucket TEXT NOT NULL,
    cache_class TEXT NOT NULL,
    import_batch_id TEXT NOT NULL,
    PRIMARY KEY (bucket_start_ns, proxy_request_id, key_id, provider, model, credential_id,
        endpoint_class, auth_type, service_tier, succeeded, error_class, status_code,
        token_quality, latency_bucket, cache_class, import_batch_id),
    CHECK(bucket_start_ns < bucket_end_ns)
);

CREATE INDEX analytics_15m_requests_range_idx ON analytics_15m_requests(bucket_start_ns, proxy_request_id);
CREATE INDEX analytics_15m_requests_key_range_idx ON analytics_15m_requests(key_id, bucket_start_ns, proxy_request_id);

WITH classified AS (
    SELECT
        requested_at_ns - ((requested_at_ns % 900000000000 + 900000000000) % 900000000000) AS bucket_start_ns,
        requested_at_ns,
        proxy_request_id,
        key_id,
        provider,
        model,
        COALESCE(credential_id, '') AS credential_id,
        endpoint_class,
        COALESCE(auth_type, '') AS auth_type,
        COALESCE(service_tier_used, service_tier_requested, '') AS service_tier,
        succeeded,
        COALESCE(error_class, '') AS error_class,
        COALESCE(upstream_status_code, 0) AS status_code,
        token_quality,
        CASE
            WHEN latency_ms < 100 THEN '<100ms'
            WHEN latency_ms < 500 THEN '100-499ms'
            WHEN latency_ms < 1000 THEN '500-999ms'
            ELSE '1000ms+'
        END AS latency_bucket,
        CASE WHEN cache_read_tokens > 0 THEN 'cached' ELSE 'uncached' END AS cache_class,
        COALESCE(import_batch_id, '') AS import_batch_id,
        input_tokens,
        output_tokens,
        reasoning_tokens,
        cached_tokens,
        cache_read_tokens,
        cache_creation_tokens,
        total_tokens,
        generation_time_ms,
        COALESCE(known_cost_nano, 0) AS known_cost_nano,
        unpriced_tokens
    FROM events
)
INSERT INTO analytics_15m (
    bucket_start_ns, bucket_end_ns, first_activity_ns, last_activity_ns,
    key_id, provider, model, credential_id, endpoint_class, auth_type, service_tier,
    succeeded, error_class, status_code, token_quality, latency_bucket, cache_class,
    import_batch_id, proxy_requests, upstream_attempts, input_tokens, output_tokens,
    reasoning_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
    generation_time_ms, generation_sample_count, known_cost_nano, unpriced_tokens)
SELECT
    bucket_start_ns, bucket_start_ns + 900000000000,
    MIN(requested_at_ns), MAX(requested_at_ns), key_id, provider, model, credential_id,
    endpoint_class, auth_type, service_tier, succeeded, error_class, status_code,
    token_quality, latency_bucket, cache_class, import_batch_id,
    COUNT(DISTINCT proxy_request_id), COUNT(*), SUM(input_tokens), SUM(output_tokens),
    SUM(reasoning_tokens), SUM(cached_tokens), SUM(cache_read_tokens), SUM(cache_creation_tokens),
    SUM(total_tokens), COALESCE(SUM(generation_time_ms), 0), COUNT(generation_time_ms),
    SUM(known_cost_nano), SUM(unpriced_tokens)
FROM classified
GROUP BY bucket_start_ns, key_id, provider, model, credential_id, endpoint_class,
    auth_type, service_tier, succeeded, error_class, status_code, token_quality,
    latency_bucket, cache_class, import_batch_id;

WITH classified AS (
    SELECT
        requested_at_ns - ((requested_at_ns % 900000000000 + 900000000000) % 900000000000) AS bucket_start_ns,
        requested_at_ns,
        proxy_request_id,
        key_id,
        provider,
        model,
        COALESCE(credential_id, '') AS credential_id,
        endpoint_class,
        COALESCE(auth_type, '') AS auth_type,
        COALESCE(service_tier_used, service_tier_requested, '') AS service_tier,
        succeeded,
        COALESCE(error_class, '') AS error_class,
        COALESCE(upstream_status_code, 0) AS status_code,
        token_quality,
        CASE
            WHEN latency_ms < 100 THEN '<100ms'
            WHEN latency_ms < 500 THEN '100-499ms'
            WHEN latency_ms < 1000 THEN '500-999ms'
            ELSE '1000ms+'
        END AS latency_bucket,
        CASE WHEN cache_read_tokens > 0 THEN 'cached' ELSE 'uncached' END AS cache_class,
        COALESCE(import_batch_id, '') AS import_batch_id
    FROM events
)
INSERT OR IGNORE INTO analytics_15m_requests (
    bucket_start_ns, bucket_end_ns, proxy_request_id, key_id, provider, model, credential_id,
    endpoint_class, auth_type, service_tier, succeeded, error_class, status_code, token_quality,
    latency_bucket, cache_class, import_batch_id)
SELECT
    bucket_start_ns, bucket_start_ns + 900000000000, proxy_request_id, key_id, provider,
    model, credential_id, endpoint_class, auth_type, service_tier, succeeded, error_class,
    status_code, token_quality, latency_bucket, cache_class, import_batch_id
FROM classified
GROUP BY bucket_start_ns, proxy_request_id, key_id, provider, model, credential_id,
    endpoint_class, auth_type, service_tier, succeeded, error_class, status_code,
    token_quality, latency_bucket, cache_class, import_batch_id;
