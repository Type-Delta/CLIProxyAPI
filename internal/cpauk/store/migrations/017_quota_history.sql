CREATE TABLE IF NOT EXISTS quota_history (
  provider TEXT NOT NULL,
  credential_id TEXT NOT NULL,
  window_id TEXT NOT NULL,
  observed_at_ns INTEGER NOT NULL,
  remaining_percent REAL NOT NULL,
  reset_at_ns INTEGER,
  period_hours REAL,
  plan_id TEXT,
  capacity REAL,
  source TEXT NOT NULL,
  PRIMARY KEY (provider, credential_id, window_id, observed_at_ns)
);
CREATE INDEX IF NOT EXISTS quota_history_range ON quota_history(provider, observed_at_ns);
