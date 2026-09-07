CREATE TABLE IF NOT EXISTS pricing_manual_rules (
    rule_id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    alias TEXT NOT NULL,
    input_per_million_nano INTEGER,
    output_per_million_nano INTEGER,
    cache_read_multiplier TEXT NOT NULL,
    cache_creation_multiplier TEXT NOT NULL,
    source TEXT NOT NULL,
    CHECK((model <> '' AND alias = '') OR (model = '' AND alias <> '')),
    CHECK((input_per_million_nano IS NULL) = (output_per_million_nano IS NULL))
);

CREATE INDEX IF NOT EXISTS pricing_manual_provider_model_idx ON pricing_manual_rules(provider, model);

CREATE TABLE IF NOT EXISTS pricing_catalog_rules (
    rule_id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    alias TEXT NOT NULL,
    input_per_million_nano INTEGER,
    output_per_million_nano INTEGER,
    cache_read_multiplier TEXT NOT NULL,
    cache_creation_multiplier TEXT NOT NULL,
    source TEXT NOT NULL,
    CHECK(provider <> ''),
    CHECK((model <> '' AND alias = '') OR (model = '' AND alias <> '')),
    CHECK((input_per_million_nano IS NULL) = (output_per_million_nano IS NULL))
);

CREATE INDEX IF NOT EXISTS pricing_catalog_provider_model_idx ON pricing_catalog_rules(provider, model);

CREATE TABLE IF NOT EXISTS pricing_catalog_provenance (
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
    source TEXT NOT NULL,
    source_digest TEXT NOT NULL,
    synced_at_ns INTEGER NOT NULL DEFAULT 0,
    expires_at_ns INTEGER NOT NULL DEFAULT 0,
    retry_at_ns INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT ''
);
