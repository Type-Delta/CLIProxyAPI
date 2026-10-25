-- The completion expression and partial predicate match the history reader.
-- Only raw, sustained observed windows enter this index; predictions are derived.
CREATE INDEX events_arrival_estimate_idx ON events (
    key_id, provider, model, executor_type, credential_id, credential_id_algorithm,
    json_extract(output_timing, '$.protocol'),
    json_extract(output_timing, '$.source'),
    json_extract(output_timing, '$.coverage'),
    stream, reasoning_effort, service_tier_requested, service_tier_used,
    max(requested_at_ns + latency_ms * 1000000,
        coalesce(responded_at_ns, requested_at_ns + latency_ms * 1000000)) DESC,
    proxy_request_id ASC, attempt_id ASC
)
WHERE succeeded = 1 AND import_batch_id IS NULL
    AND accounting_schema = 'normalized-v1' AND token_quality IN ('exact', 'estimated')
    AND output_tokens > 0 AND input_tokens <= total_tokens
    AND output_tokens <= total_tokens - input_tokens AND reasoning_tokens <= output_tokens
    AND latency_ms > 0 AND latency_ms <= 9223372036854
    AND requested_at_ns <= 9223372036854775807 - latency_ms * 1000000
    AND output_timing IS NOT NULL
    AND json_extract(output_timing, '$.version') = 1
    AND json_extract(output_timing, '$.status') = 'observed'
    AND json_extract(output_timing, '$.last_arrival_ns') - json_extract(output_timing, '$.first_arrival_ns') >= 1000000000
    AND json_extract(output_timing, '$.window_tokens') >= 64
    AND json_extract(output_timing, '$.arrival_count') >= 10
    AND json_extract(output_timing, '$.token_quality') IN ('exact', 'estimated');
