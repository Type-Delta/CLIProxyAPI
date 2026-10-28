-- Leave historical rows and aggregates unchanged; readers classify legacy rows.
ALTER TABLE events ADD COLUMN outcome TEXT CHECK(outcome IN ('succeeded', 'failed', 'canceled'));
ALTER TABLE events ADD COLUMN failure_hop TEXT CHECK(failure_hop IN ('client_to_cpa', 'cpa_to_provider', 'provider_to_cpa', 'cpa_to_client'));
ALTER TABLE events ADD COLUMN cancellation_source TEXT CHECK(cancellation_source IN ('client', 'internal', 'unknown'));
