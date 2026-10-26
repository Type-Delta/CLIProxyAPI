ALTER TABLE pricing_manual_rules ADD COLUMN service_tier_prices TEXT NOT NULL DEFAULT '{}';
ALTER TABLE pricing_catalog_rules ADD COLUMN service_tier_prices TEXT NOT NULL DEFAULT '{}';

-- Refresh the pre-tier catalog on the next demand, preserving its stale fallback.
UPDATE pricing_catalog_provenance SET expires_at_ns = 0, retry_at_ns = 0;
