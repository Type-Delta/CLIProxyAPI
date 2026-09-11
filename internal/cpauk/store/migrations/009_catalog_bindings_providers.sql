CREATE TABLE IF NOT EXISTS pricing_catalog_bindings (
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
    bindings_digest TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS pricing_catalog_providers (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL
);
