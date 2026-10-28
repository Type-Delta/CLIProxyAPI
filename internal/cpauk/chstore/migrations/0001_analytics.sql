CREATE TABLE IF NOT EXISTS {database}.analytics_events (
    retained_generation UInt64 DEFAULT 0 COMMENT 'Nonzero generation for a pending late write or compact deduplication receipt. Zero for ordinary raw events.',
    retained_grain String DEFAULT '' COMMENT 'Target hourly or daily bucket grain for a late write. Empty for ordinary raw events.',
    retained_bucket_start_ns Int64 DEFAULT 0 COMMENT 'Target bucket start for a late write, as signed UTC Unix nanoseconds.',
    attempt_id String COMMENT 'Lowercase 128-bit hex attempt identity. Read FINAL to count logical attempts.',
    schema_version Int64 COMMENT 'CPAUK Event schema version, currently 1.',
    proxy_request_id String COMMENT 'Lowercase 128-bit hex client request identity shared by retry attempts. Count DISTINCT for requests.',
    request_id_quality String COMMENT 'observed when the proxy supplied the request ID, otherwise synthetic.',
    key_id String COMMENT 'SHA256 hex of the inbound API key. Never the raw key.',
    requested_at_ns Int64 COMMENT 'Attempt start instant as signed UTC Unix nanoseconds.',
    provider String COMMENT 'CPA provider identifier associated with this attempt.',
    executor_type String COMMENT 'Runtime executor identity, distinct from the provider and requested model.',
    model String COMMENT 'Resolved upstream model identifier used for pricing and grouping.',
    requested_alias Nullable(String) COMMENT 'Client requested model alias before upstream resolution. NULL means absent.',
    endpoint_class String COMMENT 'Normalized protocol endpoint class such as responses or chat_completions.',
    auth_type Nullable(String) COMMENT 'Credential authentication class such as oauth or api_key. NULL means unknown.',
    credential_id Nullable(String) COMMENT 'HMAC credential pseudonym bound to identity.key. Never a credential secret.',
    credential_id_algorithm Nullable(String) COMMENT 'Credential pseudonym algorithm identifier.',
    session_digest Nullable(String) COMMENT 'Full pseudonymous session digest for exact distinct-session counts.',
    succeeded UInt8 COMMENT 'Compatibility success flag. Canceled attempts count as succeeded. Use outcome for cancellation.',
    upstream_status_code Nullable(Int64) COMMENT 'Upstream provider HTTP response status. NULL means no status was observed.',
    error_class Nullable(String) COMMENT 'Normalized upstream failure category. NULL means no recorded category.',
    latency_ms Int64 COMMENT 'latency ms in integer milliseconds. NULL means unavailable.',
    time_to_first_token_ms Nullable(Int64) COMMENT 'time to first token ms in integer milliseconds. NULL means unavailable.',
    service_tier_requested Nullable(String) COMMENT 'Service tier requested in the client payload. NULL means unspecified.',
    service_tier_used Nullable(String) COMMENT 'Service tier reported by the upstream provider. NULL means unreported.',
    generated UInt8 COMMENT 'Whether this attempt generated output according to usage accounting.',
    input_tokens Int64 COMMENT 'input tokens as a nonnegative count under accounting_schema.',
    output_tokens Int64 COMMENT 'output tokens as a nonnegative count under accounting_schema.',
    reasoning_tokens Int64 COMMENT 'reasoning tokens as a nonnegative count under accounting_schema.',
    cached_tokens Int64 COMMENT 'cached tokens as a nonnegative count under accounting_schema.',
    cache_read_tokens Int64 COMMENT 'cache read tokens as a nonnegative count under accounting_schema.',
    cache_creation_tokens Int64 COMMENT 'cache creation tokens as a nonnegative count under accounting_schema.',
    total_tokens Int64 COMMENT 'total tokens as a nonnegative count under accounting_schema.',
    accounting_schema String COMMENT 'Token category accounting contract, normally normalized-v1 or raw-fallback-v1.',
    token_quality String COMMENT 'exact, estimated or missing token counts. Aggregates preserve the weakest evidence.',
    known_cost_nano Nullable(Int64) COMMENT 'Known cost in integer nano-USD. NULL means no known component.',
    unpriced_tokens Int64 COMMENT 'Token count without a matching price. Keep separate from known cost.',
    price_rule_id Nullable(String) COMMENT 'Historical matched price rule identifier.',
    price_source Nullable(String) COMMENT 'Historical matched price source.',
    import_batch_id Nullable(String) COMMENT 'Import ownership identifier for rollback. NULL means live intake.',
    generation_time_ms Nullable(Int64) COMMENT 'generation time ms in integer milliseconds. NULL means unavailable.',
    client_method Nullable(String) COMMENT 'HTTP method on the client to CPA leg. NULL means not captured.',
    client_path Nullable(String) COMMENT 'Sanitized client endpoint path. NULL means not captured.',
    received_at_ns Nullable(Int64) COMMENT 'received at ns as signed UTC Unix nanoseconds.',
    upstream_method Nullable(String) COMMENT 'HTTP method on the CPA to provider leg. NULL means not captured.',
    upstream_url Nullable(String) COMMENT 'Sanitized upstream URL without credentials. NULL means not captured.',
    upstream_sent_at_ns Nullable(Int64) COMMENT 'upstream sent at ns as signed UTC Unix nanoseconds.',
    upstream_usage_raw Nullable(String) COMMENT 'Allowlisted upstream usage payload text. JSON when available.',
    upstream_error_body Nullable(String) COMMENT 'Bounded upstream failure body captured for diagnostics. NULL means absent.',
    proxy_status_code Nullable(Int64) COMMENT 'Client response HTTP status before outcomes overlay. NULL means not recorded.',
    proxy_error Nullable(String) COMMENT 'Bounded client response error before outcomes overlay.',
    responded_at_ns Nullable(Int64) COMMENT 'Client response UTC Unix nanoseconds before outcomes overlay.',
    first_token_latency_ms Nullable(Int64) COMMENT 'first token latency ms in integer milliseconds. NULL means unavailable.',
    provider_latency_ms Nullable(Int64) COMMENT 'provider latency ms in integer milliseconds. NULL means unavailable.',
    routing_time_ms Nullable(Int64) COMMENT 'routing time ms in integer milliseconds. NULL means unavailable.',
    output_timing Nullable(String) COMMENT 'JSON observed token arrival windows and coverage, not a throughput prediction.',
    stream Nullable(UInt8) COMMENT 'Whether streaming was requested. NULL means historical unknown, not false.',
    reasoning_effort Nullable(String) COMMENT 'Recorded requested reasoning effort, not proof of upstream application.',
    provider_credits Nullable(String) COMMENT 'Exact decimal provider-credit quantity as text, never a float.',
    price_usd_per_credit_nano Nullable(Int64) COMMENT 'Historical integer nano-USD per provider credit.',
    pricing_breakdown Nullable(String) COMMENT 'JSON historical pricing components, rates and matched service tier.',
    session_id Nullable(String) COMMENT 'Bounded session display identifier including source prefix when captured.',
    outcome Nullable(String) COMMENT 'Recorded succeeded, failed or canceled outcome. NULL means classify legacy evidence.',
    failure_hop Nullable(String) COMMENT 'Failed leg: client_to_cpa, cpa_to_provider, provider_to_cpa or cpa_to_client. NULL means unknown.',
    cancellation_source Nullable(String) COMMENT 'client, internal or unknown cancellation source. NULL means not recorded.',
    event_json String CODEC(ZSTD(3)) COMMENT 'Lossless Event v1 JSON with historical price. Scalar columns mirror this payload.',
    version UInt64 COMMENT 'Monotonic replacement version. Select FINAL before filtering tombstones.',
    is_deleted UInt8 COMMENT 'Logical deletion marker. Compact retained receipts use a separate partition and remain excluded from event reads.',
    INDEX attempt_id_bloom attempt_id TYPE bloom_filter(0.0001) GRANULARITY 1
) ENGINE = ReplacingMergeTree(version)
PARTITION BY if(is_deleted=1 AND retained_generation>0, toUInt32(0), toYYYYMM(fromUnixTimestamp64Nano(requested_at_ns,'UTC')))
ORDER BY (requested_at_ns, attempt_id)
COMMENT 'Versioned attempts in UTC monthly partitions, ordered by event time and attempt ID. Partition zero stores only compact replay receipts. Writers enforce global attempt identity using the bloom index. Read FINAL and exclude tombstones before overlaying response outcomes.';
CREATE TABLE IF NOT EXISTS {database}.analytics_outcomes (
    proxy_request_id String COMMENT 'Client request shared by the affected attempts.',
    attempt_ids Array(String) COMMENT 'Only attempts eligible when this patch was received. Later attempts do not inherit patches.',
    status_code Int64 COMMENT 'Upstream HTTP status used for retained grouping, or zero when unrecorded.',
    error String COMMENT 'Bounded response error. Empty means no error.',
    responded_at_ns Int64 COMMENT 'Client response UTC Unix nanoseconds.',
    sequence UInt64 COMMENT 'Monotonic arrival sequence. Earliest sequence for each affected attempt wins.'
) ENGINE = MergeTree
ORDER BY (proxy_request_id, sequence)
COMMENT 'Append-only client response patches. ARRAY JOIN attempt_ids then argMin a tuple by sequence per attempt.';
CREATE TABLE IF NOT EXISTS {database}.analytics_metadata (
    key String COMMENT 'Metadata name including identity_fingerprint, identity_epoch, retention_time_zone and checkpoints.',
    value String COMMENT 'Metadata value. Fingerprint is SHA256 hex, epoch is 128-bit hex, cutoffs are UTC Unix ns text.',
    version UInt64 COMMENT 'Monotonic replacement version. Select FINAL before filtering tombstones.',
    is_deleted UInt8 COMMENT 'Logical deletion marker. Exclude only after FINAL.'
) ENGINE = ReplacingMergeTree(version)
ORDER BY key
COMMENT 'Current analytics metadata. Read FINAL then exclude is_deleted. Identity key bytes never enter this table.';
CREATE TABLE IF NOT EXISTS {database}.analytics_control (
    namespace String COMMENT 'Collection: pricing, provider_quota, key_lifecycle, import_checkpoints or retention.',
    generation UInt64 COMMENT 'Snapshot ID. Visible only when referenced by analytics_control_heads.',
    key String COMMENT 'Collection-specific stable entity identity.',
    value String CODEC(ZSTD(3)) COMMENT 'JSON of existing CPAUK DTO. Pricing snapshot and provider list share one generation.',
    version UInt64 COMMENT 'Monotonic replacement version. Select FINAL before filtering tombstones.',
    is_deleted UInt8 COMMENT 'Logical deletion marker. Exclude only after FINAL.'
) ENGINE = ReplacingMergeTree(version)
ORDER BY (namespace, generation, key)
COMMENT 'Staged and published control snapshots. Resolve the current head once, then read that generation FINAL.';
CREATE TABLE IF NOT EXISTS {database}.analytics_control_heads (
    namespace String COMMENT 'Control collection name.',
    generation UInt64 COMMENT 'Published complete snapshot ID, including an empty collection.',
    version UInt64 COMMENT 'Monotonic replacement version. Select FINAL before filtering tombstones.'
) ENGINE = ReplacingMergeTree(version)
ORDER BY namespace
COMMENT 'Atomic publication markers for replace-all control collections. Read FINAL.';
CREATE TABLE IF NOT EXISTS {database}.analytics_rollups (
    generation UInt64 DEFAULT 0 COMMENT 'Whole bucket snapshot generation. Zero is legacy; otherwise read the generation published in analytics_retention_heads.',
    grain String COMMENT 'Retained bucket resolution, hourly or daily. Never sum both resolutions for overlapping time.',
    bucket_start_ns Int64 COMMENT 'Inclusive bucket boundary as signed UTC Unix nanoseconds in the stored retention timezone.',
    bucket_end_ns Int64 COMMENT 'Exclusive bucket boundary as signed UTC Unix nanoseconds. DST days can differ from 24 hours.',
    first_activity_ns Int64 COMMENT 'Earliest contributing attempt start as signed UTC Unix nanoseconds.',
    last_activity_ns Int64 COMMENT 'Latest contributing attempt start as signed UTC Unix nanoseconds.',
    key_id String COMMENT 'SHA256 hex of the inbound API key. Never the raw key.',
    provider String COMMENT 'CPA provider identifier associated with this attempt.',
    model String COMMENT 'Resolved upstream model identifier used for pricing and grouping.',
    credential_id String COMMENT 'HMAC credential pseudonym bound to identity.key. Never a credential secret.',
    endpoint_class String COMMENT 'Normalized protocol endpoint class such as responses or chat_completions.',
    auth_type String COMMENT 'Credential authentication class such as oauth or api_key. Empty means unknown.',
    service_tier String COMMENT 'Effective grouping tier, used tier first then requested tier, otherwise empty.',
    succeeded Int64 COMMENT 'Compatibility success flag. Canceled attempts count as succeeded. Use outcome for cancellation.',
    error_class String COMMENT 'Normalized upstream failure category. Empty means no recorded category.',
    status_code Int64 COMMENT 'Upstream HTTP status used for retained grouping, or zero when unrecorded.',
    token_quality String COMMENT 'exact, estimated or missing token counts. Aggregates preserve the weakest evidence.',
    latency_bucket String COMMENT 'Retained latency class: <100ms, 100-499ms, 500-999ms or 1000ms+.',
    cache_class String COMMENT 'cached when cache_read_tokens is positive, otherwise uncached.',
    import_batch_id String COMMENT 'Import ownership identifier for rollback. Empty means live intake.',
    proxy_requests Int64 COMMENT 'Per-dimension bucket request count. Not additive across dimensions or buckets. Use exact request identities.',
    upstream_attempts Int64 COMMENT 'Number of attempts represented by this aggregate.',
    input_tokens Int64 COMMENT 'input tokens as a nonnegative count under accounting_schema.',
    output_tokens Int64 COMMENT 'output tokens as a nonnegative count under accounting_schema.',
    reasoning_tokens Int64 COMMENT 'reasoning tokens as a nonnegative count under accounting_schema.',
    cached_tokens Int64 COMMENT 'cached tokens as a nonnegative count under accounting_schema.',
    cache_read_tokens Int64 COMMENT 'cache read tokens as a nonnegative count under accounting_schema.',
    cache_creation_tokens Int64 COMMENT 'cache creation tokens as a nonnegative count under accounting_schema.',
    total_tokens Int64 COMMENT 'total tokens as a nonnegative count under accounting_schema.',
    known_cost_nano Int64 COMMENT 'Known cost in integer nano-USD. Zero means no known component, with unpriced_tokens tracked separately.',
    unpriced_tokens Int64 COMMENT 'Token count without a matching price. Keep separate from known cost.',
    version UInt64 COMMENT 'Monotonic replacement version. Select FINAL before filtering tombstones.',
    is_deleted UInt8 COMMENT 'Logical deletion marker. Exclude only after FINAL.'
) ENGINE = ReplacingMergeTree(version)
ORDER BY (grain, bucket_start_ns, key_id, provider, model, credential_id, endpoint_class, auth_type, service_tier, succeeded, error_class, status_code, token_quality, latency_bucket, cache_class, import_batch_id, generation)
COMMENT 'Retained dimension totals. Read FINAL, exclude tombstones and select the published bucket generation. Request counts require exact request identities.';
CREATE TABLE IF NOT EXISTS {database}.analytics_request_rollups (
    generation UInt64 DEFAULT 0 COMMENT 'Whole bucket snapshot generation. Zero is legacy; otherwise read the generation published in analytics_retention_heads.',
    grain String COMMENT 'Retained bucket resolution, hourly or daily. Never sum both resolutions for overlapping time.',
    bucket_start_ns Int64 COMMENT 'Inclusive bucket boundary as signed UTC Unix nanoseconds in the stored retention timezone.',
    bucket_end_ns Int64 COMMENT 'Exclusive bucket boundary as signed UTC Unix nanoseconds. DST days can differ from 24 hours.',
    proxy_request_id String COMMENT 'Lowercase 128-bit hex client request identity shared by retry attempts. Count DISTINCT for requests.',
    key_id String COMMENT 'SHA256 hex of the inbound API key. Never the raw key.',
    provider String COMMENT 'CPA provider identifier associated with this attempt.',
    model String COMMENT 'Resolved upstream model identifier used for pricing and grouping.',
    credential_id String COMMENT 'HMAC credential pseudonym bound to identity.key. Never a credential secret.',
    endpoint_class String COMMENT 'Normalized protocol endpoint class such as responses or chat_completions.',
    auth_type String COMMENT 'Credential authentication class such as oauth or api_key. Empty means unknown.',
    service_tier String COMMENT 'Effective grouping tier, used tier first then requested tier, otherwise empty.',
    succeeded Int64 COMMENT 'Compatibility success flag. Canceled attempts count as succeeded. Use outcome for cancellation.',
    error_class String COMMENT 'Normalized upstream failure category. Empty means no recorded category.',
    status_code Int64 COMMENT 'Upstream HTTP status used for retained grouping, or zero when unrecorded.',
    token_quality String COMMENT 'exact, estimated or missing token counts. Aggregates preserve the weakest evidence.',
    latency_bucket String COMMENT 'Retained latency class: <100ms, 100-499ms, 500-999ms or 1000ms+.',
    cache_class String COMMENT 'cached when cache_read_tokens is positive, otherwise uncached.',
    import_batch_id String COMMENT 'Import ownership identifier for rollback. Empty means live intake.',
    version UInt64 COMMENT 'Monotonic replacement version. Select FINAL before filtering tombstones.',
    is_deleted UInt8 COMMENT 'Logical deletion marker. Exclude only after FINAL.'
) ENGINE = ReplacingMergeTree(version)
ORDER BY (grain, bucket_start_ns, proxy_request_id, key_id, provider, model, credential_id, endpoint_class, auth_type, service_tier, succeeded, error_class, status_code, token_quality, latency_bucket, cache_class, import_batch_id, generation)
COMMENT 'Retained exact request identities. Read FINAL, exclude tombstones and select the published bucket generation. Combine DISTINCT IDs across raw and retained history.';

CREATE TABLE IF NOT EXISTS {database}.analytics_retention_heads (
 grain String COMMENT 'Retained bucket grain, hourly or daily.',
 bucket_start_ns Int64 COMMENT 'Bucket start as signed UTC Unix nanoseconds in the retention timezone.',
 import_batch_id String COMMENT 'Exact import owner; empty for native history. Matches retained fact keys.',
 generation UInt64 COMMENT 'Published complete snapshot shared by totals and request identities.',
 version UInt64 COMMENT 'Monotonic head replacement version. Read FINAL.'
) ENGINE = ReplacingMergeTree(version)
ORDER BY (grain,bucket_start_ns,import_batch_id)
COMMENT 'Atomic bucket snapshot publication heads. Generation zero facts remain visible only when no head exists.';

CREATE TABLE IF NOT EXISTS {database}.analytics_quota_history (
    provider String COMMENT 'CPA provider identifier associated with this sanitized quota observation.',
    credential_id String COMMENT 'Full pseudonymous credential identifier, never a credential or token.',
    window_id String COMMENT 'Provider quota window identifier.',
    observed_at_ns Int64 COMMENT 'Observation instant as signed UTC Unix nanoseconds.',
    remaining_percent Float64 COMMENT 'Remaining percentage, from zero through one hundred.',
    reset_at_ns Nullable(Int64) COMMENT 'Optional quota reset instant as signed UTC Unix nanoseconds.',
    period_hours Nullable(Float64) COMMENT 'Optional quota period in hours.',
    plan_id String COMMENT 'Bounded provider plan identifier.',
    capacity Nullable(Float64) COMMENT 'Optional bounded quota capacity.',
    source String COMMENT 'Observation source, cpa_auto or cpamc.',
    row_id String COMMENT 'Stable observation identity used for retry-safe imports and precise retention.'
) ENGINE = MergeTree
ORDER BY (provider, observed_at_ns, credential_id, window_id, row_id)
COMMENT 'Quota observations. Writes prune rows older than 90 days and cap each updated provider at 100000 observations.';
