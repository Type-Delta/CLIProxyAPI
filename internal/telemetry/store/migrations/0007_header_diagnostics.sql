ALTER TABLE {database}.requests ADD INDEX IF NOT EXISTS proxy_request_id_idx proxy_request_id TYPE bloom_filter GRANULARITY 1;
ALTER TABLE {database}.requests MATERIALIZE INDEX proxy_request_id_idx;
ALTER TABLE {database}.requests COMMENT COLUMN detail 'Capture level: full, error, or headers (HTTP headers only, without bodies)';
