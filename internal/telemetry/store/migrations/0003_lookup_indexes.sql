ALTER TABLE {database}.request_contexts ADD INDEX IF NOT EXISTS request_id_idx request_id TYPE bloom_filter GRANULARITY 1;
ALTER TABLE {database}.request_contexts MATERIALIZE INDEX request_id_idx;
ALTER TABLE {database}.requests ADD INDEX IF NOT EXISTS response_id_idx response_id TYPE bloom_filter GRANULARITY 1;
ALTER TABLE {database}.requests MATERIALIZE INDEX response_id_idx;
ALTER TABLE {database}.attempts ADD INDEX IF NOT EXISTS response_id_idx response_id TYPE bloom_filter GRANULARITY 1;
ALTER TABLE {database}.attempts MATERIALIZE INDEX response_id_idx;
