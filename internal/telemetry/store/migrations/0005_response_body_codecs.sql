-- Heavier ZSTD and larger compressed blocks for the two captured-body columns, matching
-- transcript_content. Response bodies are large, highly repetitive SSE streams. Encoding-only:
-- values are unchanged and existing parts gain the codec and block size as they merge. The block
-- settings are table-wide, so point lookups in large parts decompress up to 8 MiB of the key
-- columns; that stays a small constant cost compared with the body data these tables store.
ALTER TABLE {database}.requests
    MODIFY COLUMN response_body CODEC(ZSTD(6)),
    MODIFY COLUMN websocket_timeline CODEC(ZSTD(6)),
    MODIFY SETTING min_compress_block_size = 8388608, max_compress_block_size = 8388608;
ALTER TABLE {database}.attempts
    MODIFY COLUMN response_body CODEC(ZSTD(6)),
    MODIFY COLUMN websocket_timeline CODEC(ZSTD(6)),
    MODIFY SETTING min_compress_block_size = 8388608, max_compress_block_size = 8388608;
