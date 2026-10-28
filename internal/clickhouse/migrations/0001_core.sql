CREATE TABLE IF NOT EXISTS {database}.schema_migrations (
 component String COMMENT 'Component owning the migration',
 version UInt32 COMMENT 'Migration version within the component',
 name String COMMENT 'Migration file name',
 checksum String COMMENT 'SHA-256 checksum of the migration file',
 applied_at DateTime64(3, 'UTC') COMMENT 'Time the migration was applied'
) ENGINE = MergeTree ORDER BY (component, version) COMMENT 'Applied schema migrations and their checksums';
CREATE TABLE IF NOT EXISTS {database}.cpa_jobs (
 name String COMMENT 'Unique one-time job name',
 state String COMMENT 'Job state: pending, running, done, or failed',
 detail String COMMENT 'Job details or failure explanation',
 updated_at DateTime64(9, 'UTC') COMMENT 'Last update time',
 version UInt64 COMMENT 'Monotonically increasing job version'
) ENGINE = ReplacingMergeTree(version) ORDER BY name COMMENT 'State of one-time CPA jobs';
