CREATE TABLE analytics_metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE events (
    attempt_id TEXT PRIMARY KEY CHECK(length(attempt_id) = 32),
    schema_version INTEGER NOT NULL,
    proxy_request_id TEXT NOT NULL CHECK(length(proxy_request_id) = 32),
    request_id_quality TEXT NOT NULL,
    key_id TEXT NOT NULL CHECK(length(key_id) = 64),
    requested_at_ns INTEGER NOT NULL,
    provider TEXT NOT NULL,
    executor_type TEXT NOT NULL,
    model TEXT NOT NULL,
    requested_alias TEXT,
    endpoint_class TEXT NOT NULL,
    auth_type TEXT,
    credential_id TEXT,
    credential_id_algorithm TEXT,
    succeeded INTEGER NOT NULL CHECK(succeeded IN (0, 1)),
    upstream_status_code INTEGER,
    error_class TEXT,
    latency_ms INTEGER NOT NULL CHECK(latency_ms >= 0),
    time_to_first_token_ms INTEGER,
    service_tier_requested TEXT,
    service_tier_used TEXT,
    generated INTEGER NOT NULL CHECK(generated IN (0, 1)),
    input_tokens INTEGER NOT NULL CHECK(input_tokens >= 0),
    output_tokens INTEGER NOT NULL CHECK(output_tokens >= 0),
    reasoning_tokens INTEGER NOT NULL CHECK(reasoning_tokens >= 0),
    cached_tokens INTEGER NOT NULL CHECK(cached_tokens >= 0),
    cache_read_tokens INTEGER NOT NULL CHECK(cache_read_tokens >= 0),
    cache_creation_tokens INTEGER NOT NULL CHECK(cache_creation_tokens >= 0),
    total_tokens INTEGER NOT NULL CHECK(total_tokens >= 0),
    accounting_schema TEXT NOT NULL,
    token_quality TEXT NOT NULL,
    known_cost_nano INTEGER,
    unpriced_tokens INTEGER NOT NULL CHECK(unpriced_tokens >= 0),
    price_rule_id TEXT,
    price_source TEXT,
    import_batch_id TEXT
);

CREATE INDEX events_range_idx ON events(requested_at_ns, attempt_id);
CREATE INDEX events_key_range_idx ON events(key_id, requested_at_ns, attempt_id);
CREATE INDEX events_provider_range_idx ON events(provider, requested_at_ns);
CREATE INDEX events_model_range_idx ON events(model, requested_at_ns);
CREATE INDEX events_credential_range_idx ON events(credential_id, requested_at_ns);
CREATE INDEX events_import_batch_idx ON events(import_batch_id) WHERE import_batch_id IS NOT NULL;

CREATE TABLE import_checkpoints (
    batch_id TEXT PRIMARY KEY,
    source_kind TEXT NOT NULL,
    source_fingerprint TEXT NOT NULL,
    source_offset INTEGER NOT NULL,
    chunk INTEGER NOT NULL,
    digest TEXT NOT NULL,
    completed INTEGER NOT NULL DEFAULT 0 CHECK(completed IN (0, 1)),
    rows_read INTEGER NOT NULL DEFAULT 0,
    transformed INTEGER NOT NULL DEFAULT 0,
    inserted INTEGER NOT NULL DEFAULT 0,
    skipped INTEGER NOT NULL DEFAULT 0,
    rejected INTEGER NOT NULL DEFAULT 0,
    updated_at_ns INTEGER NOT NULL
);

CREATE TABLE retention_state (
    grain TEXT PRIMARY KEY,
    completed_cutoff_ns INTEGER NOT NULL
);

CREATE TABLE rollups (
    grain TEXT NOT NULL CHECK(grain IN ('hourly', 'daily')),
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
    succeeded INTEGER NOT NULL,
    error_class TEXT NOT NULL,
    status_code INTEGER NOT NULL,
    token_quality TEXT NOT NULL,
    latency_bucket TEXT NOT NULL,
    cache_class TEXT NOT NULL,
    import_batch_id TEXT NOT NULL,
    proxy_requests INTEGER NOT NULL,
    upstream_attempts INTEGER NOT NULL,
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    reasoning_tokens INTEGER NOT NULL,
    cached_tokens INTEGER NOT NULL,
    cache_read_tokens INTEGER NOT NULL,
    cache_creation_tokens INTEGER NOT NULL,
    total_tokens INTEGER NOT NULL,
    known_cost_nano INTEGER NOT NULL,
    unpriced_tokens INTEGER NOT NULL,
    PRIMARY KEY (grain, bucket_start_ns, key_id, provider, model, credential_id, endpoint_class,
        auth_type, service_tier, succeeded, error_class, status_code, token_quality,
        latency_bucket, cache_class, import_batch_id)
);

CREATE INDEX rollups_range_idx ON rollups(grain, bucket_start_ns);
CREATE INDEX rollups_key_range_idx ON rollups(grain, key_id, bucket_start_ns);
CREATE INDEX rollups_import_batch_idx ON rollups(import_batch_id) WHERE import_batch_id <> '';

CREATE TABLE request_rollups (
    grain TEXT NOT NULL CHECK(grain IN ('hourly', 'daily')),
    bucket_start_ns INTEGER NOT NULL,
    bucket_end_ns INTEGER NOT NULL,
    proxy_request_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    credential_id TEXT NOT NULL,
    endpoint_class TEXT NOT NULL,
    auth_type TEXT NOT NULL,
    service_tier TEXT NOT NULL,
    succeeded INTEGER NOT NULL,
    error_class TEXT NOT NULL,
    status_code INTEGER NOT NULL,
    token_quality TEXT NOT NULL,
    latency_bucket TEXT NOT NULL,
    cache_class TEXT NOT NULL,
    import_batch_id TEXT NOT NULL,
    PRIMARY KEY (grain, bucket_start_ns, proxy_request_id, key_id, provider, model,
        credential_id, endpoint_class, auth_type, service_tier, succeeded,
        error_class, status_code, token_quality, latency_bucket, cache_class, import_batch_id)
);

CREATE INDEX request_rollups_range_idx ON request_rollups(grain, bucket_start_ns, proxy_request_id);
CREATE INDEX request_rollups_key_range_idx ON request_rollups(grain, key_id, bucket_start_ns, proxy_request_id);
CREATE INDEX request_rollups_import_batch_idx ON request_rollups(import_batch_id) WHERE import_batch_id <> '';

CREATE TABLE pricing_rules (
    rule_id TEXT PRIMARY KEY,
    model TEXT NOT NULL,
    alias TEXT NOT NULL,
    input_per_million_nano INTEGER,
    output_per_million_nano INTEGER,
    cache_read_multiplier TEXT NOT NULL,
    cache_creation_multiplier TEXT NOT NULL,
    source TEXT NOT NULL,
    CHECK ((model <> '' AND alias = '') OR (model = '' AND alias <> '')),
    CHECK ((input_per_million_nano IS NULL) = (output_per_million_nano IS NULL))
);

CREATE TABLE pricing_provenance (
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
    source TEXT NOT NULL,
    source_digest TEXT NOT NULL,
    synced_at_ns INTEGER NOT NULL
);

CREATE TABLE provider_quota_snapshots (
    provider TEXT NOT NULL,
    credential_id TEXT NOT NULL,
    model TEXT NOT NULL,
    available INTEGER NOT NULL CHECK(available IN (0, 1)),
    disabled INTEGER NOT NULL CHECK(disabled IN (0, 1)),
    quota_exceeded INTEGER NOT NULL CHECK(quota_exceeded IN (0, 1)),
    next_reset_at_ns INTEGER,
    observed_at_ns INTEGER NOT NULL,
    PRIMARY KEY (provider, credential_id, model)
);

CREATE INDEX provider_quota_observed_idx ON provider_quota_snapshots(observed_at_ns, provider);

CREATE TABLE key_lifecycle (
    key_id TEXT PRIMARY KEY CHECK(length(key_id) = 64),
    status TEXT NOT NULL CHECK(status IN ('configured', 'rotated', 'deleted')),
    updated_at_ns INTEGER NOT NULL
);
