ALTER TABLE events ADD COLUMN first_token_latency_ms INTEGER CHECK(first_token_latency_ms >= 0);
ALTER TABLE events ADD COLUMN provider_latency_ms INTEGER CHECK(provider_latency_ms >= 0);
