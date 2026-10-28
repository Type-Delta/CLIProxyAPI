CREATE TABLE IF NOT EXISTS {database}.requests
(
    request_id String COMMENT 'Stable request log identifier; join attempts and contexts on this value',
    proxy_request_id String COMMENT 'Identifier shared with analytics events',
    started_at DateTime64(3, 'UTC') COMMENT 'Client request start time',
    first_chunk_at Nullable(DateTime64(3, 'UTC')) COMMENT 'First client response byte time when captured',
    finished_at Nullable(DateTime64(3, 'UTC')) COMMENT 'Client response completion time when captured',
    method LowCardinality(String) COMMENT 'Client HTTP method',
    url String COMMENT 'Masked client request URL',
    path String COMMENT 'Client URL path',
    client_format LowCardinality(String) COMMENT 'Transcript protocol used by the client',
    model String COMMENT 'Model requested by the client',
    stream Bool COMMENT 'Whether the client requested streaming',
    status UInt16 COMMENT 'Client response status',
    actor_key_id String COMMENT 'SHA256 identifier of the client key, never the raw key',
    session_id String COMMENT 'Client session identifier when known',
    detail LowCardinality(String) COMMENT 'Capture level: full or error',
    source LowCardinality(String) COMMENT 'Capture source: live or legacy_log',
    transport LowCardinality(String) COMMENT 'Client transport',
    request_headers String COMMENT 'JSON object of masked client request headers',
    response_headers String COMMENT 'JSON object of masked client response headers',
    envelope_content_id String COMMENT 'Join transcript_content for the envelope; rebuild using ordered client request_contexts entry_ids',
    response_id String COMMENT 'Provider response identifier for continuation lookup',
    response_body String CODEC(ZSTD(3)) COMMENT 'Captured client response including streaming framing',
    response_truncated Bool COMMENT 'Whether client response capture was truncated',
    websocket_timeline String CODEC(ZSTD(3)) COMMENT 'Captured client websocket timeline',
    errors Array(String) COMMENT 'Recorded client request errors',
    request_body_raw String CODEC(ZSTD(3)) COMMENT 'Raw client body only when it cannot be parsed as a transcript',
    ingested_at DateTime64(3, 'UTC') COMMENT 'Time the record entered ClickHouse',
    INDEX request_id_idx request_id TYPE bloom_filter GRANULARITY 1
) ENGINE = ReplacingMergeTree PARTITION BY toYYYYMM(started_at) ORDER BY (session_id, started_at, request_id)
COMMENT 'Client requests. Join attempts by request_id and contexts to rebuild request transcripts. Count distinct request_id.';

CREATE TABLE IF NOT EXISTS {database}.attempts
(
    request_id String COMMENT 'Join requests by request_id',
    attempt_index UInt32 COMMENT 'One-based upstream attempt number',
    request_started_at DateTime64(3, 'UTC') COMMENT 'Client request start time',
    started_at DateTime64(3, 'UTC') COMMENT 'Upstream attempt start time',
    response_at Nullable(DateTime64(3, 'UTC')) COMMENT 'Upstream response time when known',
    method LowCardinality(String) COMMENT 'Upstream HTTP method',
    url String COMMENT 'Masked upstream URL',
    upstream_format LowCardinality(String) COMMENT 'Upstream transcript protocol',
    provider LowCardinality(String) COMMENT 'Upstream provider name',
    auth_id String COMMENT 'Upstream credential identifier',
    auth_label String COMMENT 'Upstream credential label',
    auth_type String COMMENT 'Upstream credential type',
    transport LowCardinality(String) COMMENT 'Upstream transport',
    request_headers String COMMENT 'JSON object of masked upstream request headers',
    envelope_content_id String COMMENT 'Join transcript_content and ordered upstream context to rebuild the request body',
    request_body_raw String CODEC(ZSTD(3)) COMMENT 'Raw body only when it cannot be parsed as a transcript',
    status UInt16 COMMENT 'Upstream response status',
    response_headers String COMMENT 'JSON object of masked upstream response headers',
    response_body String CODEC(ZSTD(3)) COMMENT 'Captured upstream response including streaming framing',
    response_truncated Bool COMMENT 'Whether upstream response capture was truncated',
    websocket_timeline String CODEC(ZSTD(3)) COMMENT 'Captured upstream websocket timeline',
    errors Array(String) COMMENT 'Recorded upstream errors',
    response_id String DEFAULT '' COMMENT 'Upstream response identifier used to link previous_response_id continuations'
) ENGINE = ReplacingMergeTree PARTITION BY toYYYYMM(request_started_at) ORDER BY (request_id, attempt_index)
COMMENT 'Upstream attempts for each client request. Count distinct request_id and attempt_index.';

CREATE TABLE IF NOT EXISTS {database}.request_contexts
(
    request_id String COMMENT 'Join requests by request_id',
    attempt_index UInt32 COMMENT 'Zero for client view, one-based for upstream attempt',
    view LowCardinality(String) COMMENT 'Client or upstream transcript view',
    session_id String COMMENT 'Client session identifier when known',
    started_at DateTime64(3, 'UTC') COMMENT 'Client request start time',
    entry_ids Array(String) COMMENT 'Ordered occurrence IDs parallel to content_ids; empty string means no semantic entry. Rebuild from content_ids, not entries',
    content_ids Array(String) DEFAULT [] COMMENT 'Exact content ID at each request position parallel to entry_ids; use these IDs to rebuild the original request',
    response_head_id String DEFAULT '' COMMENT 'Last nonempty v2 occurrence after the response, used for previous_response_id continuations; call anchors resynchronize the chain',
    response_depth UInt32 DEFAULT 0 COMMENT 'Next chain depth after the response head, used for continuations'
) ENGINE = ReplacingMergeTree PARTITION BY toYYYYMM(started_at) ORDER BY (session_id, started_at, request_id, attempt_index, view)
COMMENT 'Ordered request transcript references. Join entry_ids to transcript_entries and content, preserving array order.';

CREATE TABLE IF NOT EXISTS {database}.transcript_entries
(
    entry_id String COMMENT 'v2 occurrence ID: unique provider calls/results anchor to actor, protocol, role, kind and sorted call IDs; other content hashes its parent and semantics. Count distinct entry_id in the client view',
    parent_entry_id String COMMENT 'Previous nonempty entry or v2 protocol root at first observation. Call anchors do not depend on this parent',
    depth UInt32 COMMENT 'Zero-based semantic position at first observation; empty elements do not advance it',
    actor_key_id String COMMENT 'Pseudonymous client key identifier',
    session_id String COMMENT 'Session where this occurrence was first observed',
    format LowCardinality(String) COMMENT 'Transcript protocol',
    role LowCardinality(String) COMMENT 'Conversation role',
    item_type LowCardinality(String) COMMENT 'Protocol item type',
    block_types Array(String) COMMENT 'Content block types',
    tool_call_ids Array(String) COMMENT 'Tool call identifiers; use has for lookup',
    tool_names Array(String) COMMENT 'Tool names',
    content_id String COMMENT 'Join transcript_content for exact raw element and searchable text',
    semantic_id String COMMENT 'Canonical semantic content hash; call anchors keep identity even if earlier content drifts',
    origin LowCardinality(String) COMMENT 'Whether first observed in request or response',
    first_observed_at DateTime64(3, 'UTC') COMMENT 'First capture time, even for pre-existing history',
    occurred_at Nullable(DateTime64(3, 'UTC')) COMMENT 'NULL means pre-existing history first seen later; use non-NULL times for occurrence counts',
    first_request_id String COMMENT 'Request in which this occurrence was first seen',
    first_attempt_index UInt32 COMMENT 'Zero for client view or one-based upstream attempt',
    INDEX content_idx content_id TYPE bloom_filter GRANULARITY 1,
    INDEX tool_idx tool_call_ids TYPE bloom_filter GRANULARITY 1,
    INDEX observed_idx first_observed_at TYPE minmax GRANULARITY 1
) ENGINE = ReplacingMergeTree ORDER BY (entry_id)
COMMENT 'Distinct conversation occurrences in a hash chain. Count distinct entry_id, not rows or content_id.';

CREATE TABLE IF NOT EXISTS {database}.transcript_content
(
    content_id String COMMENT 'Content hash; count distinct content_id to account for replacing merges',
    kind LowCardinality(String) COMMENT 'Element or request envelope',
    raw String CODEC(ZSTD(3)) COMMENT 'Exact JSON bytes for reconstruction',
    text String CODEC(ZSTD(3)) COMMENT 'Searchable text projection; use substring predicates and count distinct entry_id after joining entries',
    bytes UInt64 COMMENT 'Uncompressed raw byte count',
    first_observed_at DateTime64(3, 'UTC') COMMENT 'First capture time for this content',
    INDEX text_idx text TYPE ngrambf_v1(3, 10000, 3, 7) GRANULARITY 1
) ENGINE = ReplacingMergeTree ORDER BY (content_id)
COMMENT 'Content-addressed elements and envelopes. Search text and join entries to count distinct occurrences.';
