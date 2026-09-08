ALTER TABLE daily_stats ADD COLUMN known_cost_nano INTEGER NOT NULL DEFAULT 0;
ALTER TABLE daily_stats ADD COLUMN unpriced_tokens INTEGER NOT NULL DEFAULT 0 CHECK(unpriced_tokens >= 0);
