-- Historical pricing details and session identifiers were not recorded.
ALTER TABLE events ADD COLUMN pricing_breakdown TEXT;
ALTER TABLE events ADD COLUMN session_id TEXT;
CREATE INDEX events_session_id_idx ON events(session_id, requested_at_ns DESC, attempt_id);
