CREATE TABLE daily_stats (
    day_start_ns INTEGER NOT NULL,
    day_end_ns INTEGER NOT NULL,
    key_id TEXT NOT NULL,
    requests INTEGER NOT NULL CHECK(requests >= 0),
    succeeded INTEGER NOT NULL CHECK(succeeded >= 0),
    failed INTEGER NOT NULL CHECK(failed >= 0),
    input_tokens INTEGER NOT NULL CHECK(input_tokens >= 0),
    output_tokens INTEGER NOT NULL CHECK(output_tokens >= 0),
    reasoning_tokens INTEGER NOT NULL CHECK(reasoning_tokens >= 0),
    cached_tokens INTEGER NOT NULL CHECK(cached_tokens >= 0),
    cache_read_tokens INTEGER NOT NULL CHECK(cache_read_tokens >= 0),
    cache_creation_tokens INTEGER NOT NULL CHECK(cache_creation_tokens >= 0),
    total_tokens INTEGER NOT NULL CHECK(total_tokens >= 0),
    PRIMARY KEY (day_start_ns, key_id),
    CHECK(day_start_ns < day_end_ns)
);

CREATE INDEX daily_stats_range_idx ON daily_stats(day_start_ns, day_end_ns);
